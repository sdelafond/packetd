package geoip

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/geoip2-golang"
	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/dispatch"
	"github.com/untangle/packetd/services/logger"
	"github.com/untangle/packetd/services/reports"
)

const pluginName = "geoip"

var geoDatabase *geoip2.Reader
var geoMutex sync.Mutex
var privateIPBlocks []*net.IPNet

// PluginStartup is called to allow plugin specific initialization.
// We initialize an instance of the GeoIP engine using any existing
// database we can find, or we download if needed. We increment the
// argumented WaitGroup so the main process can wait for our shutdown function
// to return during shutdown.
func PluginStartup() {
	var filename string

	logger.Info("PluginStartup(%s) has been called\n", pluginName)
	geoMutex.Lock()
	defer geoMutex.Unlock()

	// start by looking for the NGFW city database file
	db, err := geoip2.Open(findGeoFile(true))
	if err != nil {
		logger.Warn("Unable to load GeoIP Database: %s\n", err)
	} else {
		logger.Info("Loading GeoIP Database: %s\n", filename)
		geoDatabase = db
	}

	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local addr
	} {
		_, block, _ := net.ParseCIDR(cidr)
		privateIPBlocks = append(privateIPBlocks, block)
	}
	dispatch.InsertNfqueueSubscription(pluginName, dispatch.GeoipPriority, PluginNfqueueHandler)
}

// PluginShutdown is called when the daemon is shutting down. We close our
// GeoIP engine and call done for the argumented WaitGroup to let the main
// process know we're finished.
func PluginShutdown() {
	logger.Info("PluginShutdown(%s) has been called\n", pluginName)
	geoMutex.Lock()
	defer geoMutex.Unlock()

	if geoDatabase != nil {
		geoDatabase.Close()
		geoDatabase = nil
	}
}

// PluginNfqueueHandler is called to handle nfqueue packet data. We extract
// the source and destination IP address from the packet, lookup the GeoIP
// country code for each, and store them in the conntrack dictionary.
func PluginNfqueueHandler(mess dispatch.NfqueueMessage, ctid uint32, newSession bool) dispatch.NfqueueResult {
	var result dispatch.NfqueueResult

	// release immediately as we only care about the first packet
	dispatch.ReleaseSession(mess.Session, pluginName)

	// we only do the lookup for new sessions
	if !newSession {
		return result
	}

	geoMutex.Lock()
	defer geoMutex.Unlock()

	// we start by setting both the client and server country to XU for unknown
	var clientCountry = "XU"
	var serverCountry = "XU"
	var srcAddr net.IP
	var dstAddr net.IP

	if mess.IP6Layer != nil {
		srcAddr = mess.IP6Layer.SrcIP
		dstAddr = mess.IP6Layer.DstIP
	}

	if mess.IP4Layer != nil {
		srcAddr = mess.IP4Layer.SrcIP
		dstAddr = mess.IP4Layer.DstIP
	}

	// first we check to see if the source or destination addresses are
	// in private address blocks and if so assign the XL local country code

	if srcAddr != nil && isPrivateIP(srcAddr) {
		clientCountry = "XL"
	}

	if dstAddr != nil && isPrivateIP(dstAddr) {
		serverCountry = "XL"
	}

	// if we have a good database and good addresses and the country
	// is still unknown we do the database lookup

	if geoDatabase != nil && srcAddr != nil && clientCountry == "XU" {
		SrcRecord, err := geoDatabase.City(srcAddr)
		if (err == nil) && (len(SrcRecord.Country.IsoCode) != 0) {
			clientCountry = SrcRecord.Country.IsoCode
		}
	}

	if geoDatabase != nil && dstAddr != nil && serverCountry == "XU" {
		DstRecord, err := geoDatabase.City(dstAddr)
		if (err == nil) && (len(DstRecord.Country.IsoCode) != 0) {
			serverCountry = DstRecord.Country.IsoCode
		}
	}

	logger.Debug("SRC: %v = %s ctid:%d\n", srcAddr, clientCountry, ctid)
	logger.Debug("DST: %v = %s ctid:%d\n", dstAddr, serverCountry, ctid)

	dict.AddSessionEntry(ctid, "client_country", clientCountry)
	dict.AddSessionEntry(ctid, "server_country", serverCountry)
	mess.Session.PutAttachment("client_country", clientCountry)
	mess.Session.PutAttachment("server_country", serverCountry)

	logEvent(mess.Session, clientCountry, serverCountry)

	return result
}

func isPrivateIP(ip net.IP) bool {
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func databaseDownload(filename string) {
	logger.Info("Downloading GeoIP Database...\n")

	// Make sure the target directory exists
	marker := strings.LastIndex(filename, "/")

	// Get the index of the last slash so we can isolate the path and create the directory
	if marker > 0 {
		os.MkdirAll(filename[0:marker], 0755)
	}

	// Get the GeoIP database from MaxMind
	resp, err := http.Get("http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country.mmdb.gz")
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		logger.Warn("Download failure: %s\n", resp.Status)
		return
	}

	// Create a reader for the compressed data
	reader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return
	}
	defer reader.Close()

	// Create the output file
	writer, err := os.Create(filename)
	if err != nil {
		return
	}
	defer writer.Close()

	// Write the uncompressed database to the file
	io.Copy(writer, reader)
	logger.Info("Downloaded GeoIP Database.\n")
}

// findGeoFile finds the location of the GeoLite2-City.mmdb file
// it checks several common locations and returns any found
// if none found it just returns "/tmp/GeoLite2-City.mmdb"
func findGeoFile(download bool) string {
	possibleLocations := []string{
		"/var/cache/untangle-geoip/GeoLite2-City.mmdb",
		"/tmp/GeoLite2-City.mmdb",
		"/usr/lib/GeoLite2-City.mmdb",
		"/usr/share/untangle-geoip/GeoLite2-Country.mmdb",
		"/usr/share/geoip/GeoLite2-Country.mmdb",
	}

	for _, filename := range possibleLocations {
		_, err := os.Stat(filename)
		if os.IsNotExist(err) {
			continue
		} else {
			return filename
		}
	}

	// If we reach this point it was not found
	if download {
		databaseDownload("/usr/lib/GeoLite2-City.mmdb")
		// try again now that we tried to download
		// but do not download again
		return findGeoFile(false)
	}

	// Not found - just return one
	return "/tmp/GeoLite2-City.mmdb"
}

// logEvent logs an update event that updates the *_country columns
// provide the session, and the client and server country
func logEvent(session *dispatch.Session, clientCountry string, serverCountry string) {
	columns := map[string]interface{}{
		"session_id": session.GetSessionID(),
	}

	modifiedColumns := make(map[string]interface{})
	modifiedColumns["client_country"] = clientCountry
	modifiedColumns["server_country"] = serverCountry

	reports.LogEvent(reports.CreateEvent("session_geoip", "sessions", 2, columns, modifiedColumns))
}
