package restd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/untangle/packetd/services/certmanager"
	"github.com/untangle/packetd/services/dispatch"
	"github.com/untangle/packetd/services/kernel"
	"github.com/untangle/packetd/services/logger"
	"github.com/untangle/packetd/services/overseer"
	"github.com/untangle/packetd/services/reports"
	"github.com/untangle/packetd/services/settings"
)

var engine *gin.Engine

// Startup is called to start the rest daemon
func Startup() {

	gin.SetMode(gin.ReleaseMode)
	gin.DisableConsoleColor()
	gin.DefaultWriter = logger.NewLogWriter()
	gin.DebugPrintRouteFunc = func(httpMethod, absolutePath, handlerName string, nuHandlers int) {
		logger.Info("GIN: %v %v %v %v\n", httpMethod, absolutePath, handlerName, nuHandlers)
	}

	engine = gin.New()
	engine.Use(ginlogger())
	engine.Use(gin.Recovery())
	engine.Use(addHeaders)

	// Allow cross-site for dev - this should be disabled in production
	// config := cors.DefaultConfig()
	// config.AllowAllOrigins = true
	// engine.Use(cors.New(config))

	// A server-side store would be better IMO, but I can't find one.
	// -dmorris
	store := cookie.NewStore([]byte(GenerateRandomString(32)))
	// store := cookie.NewStore([]byte("secret"))

	engine.Use(sessions.Sessions("auth_session", store))
	engine.Use(addTokenToSession)

	engine.GET("/", rootHandler)

	engine.GET("/ping", pingHandler)

	engine.POST("/account/login", authLogin)
	//engine.GET("/account/login", authLogin)
	engine.POST("/account/logout", authLogout)
	engine.GET("/account/logout", authLogout)
	engine.GET("/account/status", authStatus)

	api := engine.Group("/api")
	api.Use(authRequired(engine))

	api.GET("/settings", getSettings)
	api.GET("/settings/*path", getSettings)
	api.POST("/settings", setSettings)
	api.POST("/settings/*path", setSettings)
	api.DELETE("/settings", trimSettings)
	api.DELETE("/settings/*path", trimSettings)

	api.GET("/logging/:logtype", getLogOutput)

	api.GET("/defaults", getDefaultSettings)
	api.GET("/defaults/*path", getDefaultSettings)

	api.POST("/reports/create_query", reportsCreateQuery)
	api.GET("/reports/get_data/:query_id", reportsGetData)
	api.POST("/reports/close_query/:query_id", reportsCloseQuery)

	api.POST("/warehouse/capture", warehouseCapture)
	api.POST("/warehouse/close", warehouseClose)
	api.POST("/warehouse/playback", warehousePlayback)
	api.POST("/warehouse/cleanup", warehouseCleanup)
	api.GET("/warehouse/status", warehouseStatus)
	api.POST("/control/traffic", trafficControl)

	api.GET("/status/sessions", statusSessions)
	api.GET("/status/system", statusSystem)
	api.GET("/status/hardware", statusHardware)
	api.GET("/status/upgrade", statusUpgradeAvailable)
	api.GET("/status/build", statusBuild)
	api.GET("/status/wantest/:device", statusWANTest)
	api.GET("/status/uid", statusUID)
	api.GET("/status/interfaces/:device", statusInterfaces)
	api.GET("/status/arp/", statusArp)
	api.GET("/status/arp/:device", statusArp)
	api.GET("/status/dhcp", statusDHCP)
	api.GET("/status/route", statusRoute)
	api.GET("/status/routetables", statusRouteTables)
	api.GET("/status/route/:table", statusRoute)
	api.GET("/status/rules", statusRules)
	api.GET("/status/routerules", statusRouteRules)
	api.GET("/status/wwan/:device", statusWwan)
	api.GET("/status/wifichannels/:device", statusWifiChannels)
	api.GET("/status/wifimodelist/:device", statusWifiModelist)

	api.GET("/logger/:source", loggerHandler)
	api.GET("/debug", debugHandler)
	api.POST("/gc", gcHandler)

	api.POST("/sysupgrade", sysupgradeHandler)
	api.POST("/upgrade", upgradeHandler)

	api.POST("/releasedhcp/:device", releaseDhcp)
	api.POST("/renewdhcp/:device", renewDhcp)
	// files
	engine.Static("/admin", "/www/admin")
	engine.Static("/settings", "/www/settings")
	engine.Static("/reports", "/www/reports")
	engine.Static("/setup", "/www/setup")
	engine.Static("/static", "/www/static")

	prof := engine.Group("/pprof")
	prof.Use(authRequired(engine))

	prof.GET("/", pprofHandler(pprof.Index))
	prof.GET("/cmdline", pprofHandler(pprof.Cmdline))
	prof.GET("/profile", pprofHandler(pprof.Profile))
	prof.POST("/symbol", pprofHandler(pprof.Symbol))
	prof.GET("/symbol", pprofHandler(pprof.Symbol))
	prof.GET("/trace", pprofHandler(pprof.Trace))
	prof.GET("/block", pprofHandler(pprof.Handler("block").ServeHTTP))
	prof.GET("/goroutine", pprofHandler(pprof.Handler("goroutine").ServeHTTP))
	prof.GET("/heap", pprofHandler(pprof.Handler("heap").ServeHTTP))
	prof.GET("/mutex", pprofHandler(pprof.Handler("mutex").ServeHTTP))
	prof.GET("/threadcreate", pprofHandler(pprof.Handler("threadcreate").ServeHTTP))

	// listen and serve on 0.0.0.0:80
	go engine.Run(":80")

	cert, key := certmanager.GetConfiguredCert()
	go engine.RunTLS(":443", cert, key)

	logger.Info("The RestD engine has been started\n")
}

// Shutdown restd
func Shutdown() {
	return
}

// GenerateRandomString generates a random string of the specified length
func GenerateRandomString(n int) string {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		logger.Info("Failed to generated secure key: %v\n", err)
		return "secret"
	}
	return base64.URLEncoding.EncodeToString(b)
}

// RemoveEmptyStrings removes and empty strings from the string slice and returns a new slice
func RemoveEmptyStrings(strings []string) []string {
	b := strings[:0]
	for _, x := range strings {
		if x != "" {
			b = append(b, x)
		}
	}
	return b
}

func rootHandler(c *gin.Context) {
	if isSetupWizardCompleted() {
		c.Redirect(http.StatusTemporaryRedirect, "/admin")
	} else {
		c.Redirect(http.StatusTemporaryRedirect, "/setup")
	}
}

func pingHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "pong",
	})
}

// loggerHandler handles getting and setting the log level for the different logger sources
func loggerHandler(c *gin.Context) {
	queryStr := c.Param("source")
	if queryStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "missing logger source"})
		return
	}

	// split passed query on equal character to get the function arguments
	info := strings.Split(queryStr, "=")

	// we expect either one or two arguments
	if len(info) < 1 || len(info) > 2 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid logger syntax"})
	}

	// single argument is a level query
	if len(info) == 1 {
		level := logger.SearchSourceLogLevel(info[0])
		if level < 0 {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid log source specified"})
		} else {
			c.JSON(http.StatusOK, gin.H{
				"source": info[0],
				"level":  level,
			})
		}
		return
	}

	// two arguments is a request to adjust the level of a source so
	// start by finding the numeric level for the level name
	setlevel := logger.FindLogLevelName(info[1])
	if setlevel < 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid log level specified"})
		return
	}

	nowlevel := logger.AdjustSourceLogLevel(info[0], setlevel)
	if nowlevel < 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid log source specified"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"source":   info[0],
		"oldlevel": nowlevel,
		"newlevel": setlevel,
	})
}

func debugHandler(c *gin.Context) {
	var buffer bytes.Buffer
	buffer = overseer.GenerateReport()
	c.Data(http.StatusOK, "text/html; chareset=utf-8", buffer.Bytes())
}

func gcHandler(c *gin.Context) {
	logger.Info("Calling FreeOSMemory()...\n")
	debug.FreeOSMemory()
}

func pprofHandler(h http.HandlerFunc) gin.HandlerFunc {
	handler := http.HandlerFunc(h)
	return func(c *gin.Context) {
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

func reportsGetData(c *gin.Context) {
	queryStr := c.Param("query_id")
	if queryStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query_id not found"})
		return
	}
	queryID, err := strconv.ParseUint(queryStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	str, err := reports.GetData(queryID)
	if err != nil {
		//c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		// FIXME the UI pukes if you respond with 500 currently
		// once its fixed, we should change this back
		c.JSON(http.StatusOK, gin.H{"error": err})
		return
	}

	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, str)
	return
}

func reportsCreateQuery(c *gin.Context) {
	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	q, err := reports.CreateQuery(string(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	str := fmt.Sprintf("%v", q.ID)
	logger.Debug("CreateQuery(%s)\n", str)
	c.String(http.StatusOK, str)
}

func reportsCloseQuery(c *gin.Context) {
	queryStr := c.Param("query_id")
	if queryStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query_id not found"})
		return
	}
	queryID, err := strconv.ParseUint(queryStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	str, err := reports.CloseQuery(queryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	c.String(http.StatusOK, str)
	return
}

func warehousePlayback(c *gin.Context) {
	var data map[string]string
	var body []byte
	var filename string
	var speedstr string
	var speedval int
	var found bool
	var err error

	body, err = ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	err = json.Unmarshal(body, &data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	filename, found = data["filename"]
	if found != true {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "filename not specified"})
		return
	}

	speedstr, found = data["speed"]
	if found == true {
		speedval, err = strconv.Atoi(speedstr)
		if err != nil {
			speedval = 1
		}
	} else {
		speedval = 1
	}

	kernel.SetWarehouseFlag('P')
	kernel.SetWarehouseFile(filename)
	kernel.SetWarehouseSpeed(speedval)

	logger.Info("Beginning playback of file:%s speed:%d\n", filename, speedval)
	dispatch.HandleWarehousePlayback()

	c.JSON(http.StatusOK, "Playback started")
}

func warehouseCleanup(c *gin.Context) {
	dispatch.HandleWarehouseCleanup()
	c.JSON(http.StatusOK, "Cleanup success\n")
}

func warehouseCapture(c *gin.Context) {

	var data map[string]string
	var body []byte
	var filename string
	var found bool
	var err error

	body, err = ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	err = json.Unmarshal(body, &data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	filename, found = data["filename"]
	if found != true {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "filename not specified"})
		return
	}

	kernel.SetWarehouseFlag('C')
	kernel.SetWarehouseFile(filename)
	kernel.StartWarehouseCapture()

	logger.Info("Beginning capture to file:%s\n", filename)

	c.JSON(http.StatusOK, "Capture started")
}

func warehouseClose(c *gin.Context) {
	kernel.CloseWarehouseCapture()
	kernel.SetWarehouseFlag('I')

	c.JSON(http.StatusOK, "Capture finished\n")
}

func warehouseStatus(c *gin.Context) {
	var status string

	status = "UNKNOWN"
	flag := kernel.GetWarehouseFlag()
	switch flag {
	case 'I':
		status = "IDLE"
		break
	case 'P':
		status = "PLAYBACK"
		break
	case 'C':
		status = "CAPTURE"
		break
	}
	c.JSON(http.StatusOK, status)
}

func trafficControl(c *gin.Context) {
	var data map[string]string
	var body []byte
	var bypass string
	var found bool
	var err error

	body, err = ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	err = json.Unmarshal(body, &data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	bypass, found = data["bypass"]
	if found == true {
		if strings.EqualFold(bypass, "TRUE") {
			logger.Info("Setting traffic bypass flag\n")
			kernel.SetBypassFlag(1)
			c.JSON(http.StatusOK, "Traffic bypass flag ENABLED")
		} else if strings.EqualFold(bypass, "FALSE") {
			logger.Info("Clearing traffic bypass flag\n")
			kernel.SetBypassFlag(0)
			c.JSON(http.StatusOK, "Traffic bypass flag CLEARED")
		} else {
			c.JSON(http.StatusOK, gin.H{"error": "Parameter must be TRUE or FALSE"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"error": "Invalid or missing traffic control command"})
}

func getSettings(c *gin.Context) {
	var segments []string

	path := c.Param("path")

	if path == "" {
		segments = nil
	} else {
		segments = RemoveEmptyStrings(strings.Split(path, "/"))
	}

	jsonResult, err := settings.GetSettings(segments)
	if err != nil {
		c.JSON(http.StatusInternalServerError, jsonResult)
	} else {
		c.JSON(http.StatusOK, jsonResult)
	}
	return
}

func getDefaultSettings(c *gin.Context) {
	var segments []string

	path := c.Param("path")

	if path == "" {
		segments = nil
	} else {
		segments = RemoveEmptyStrings(strings.Split(path, "/"))
	}

	jsonResult, err := settings.GetDefaultSettings(segments)
	if err != nil {
		c.JSON(http.StatusInternalServerError, jsonResult)
	} else {
		c.JSON(http.StatusOK, jsonResult)
	}
	return
}

// getLogOutput will take a logtype param (ie: dmesg, logread, syslog) and attempt to retrieve the log output for that logtype, or default to logread
func getLogOutput(c *gin.Context) {

	var logcmd string

	switch logtype := c.Param("logtype"); logtype {
	case "dmesg":
		logcmd = "/bin/dmesg"
	case "syslog":
		logcmd = "cat /var/log/syslog"
	default:
		logcmd = "/sbin/logread"
	}

	output, err := exec.Command(logcmd).CombinedOutput()

	if err != nil {
		logger.Err("Error getting log output from %s: %v\n", logcmd, string(output))
		c.JSON(http.StatusInternalServerError, gin.H{"error": string(output)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"logresults": output})
	return
}

func setSettings(c *gin.Context) {
	var segments []string
	path := c.Param("path")

	if path == "" {
		segments = nil
	} else {
		segments = RemoveEmptyStrings(strings.Split(path, "/"))
	}

	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	var bodyJSONObject interface{}
	err = json.Unmarshal(body, &bodyJSONObject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
	}

	jsonResult, err := settings.SetSettings(segments, bodyJSONObject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, jsonResult)
	} else {
		c.JSON(http.StatusOK, jsonResult)
	}
	return
}

func trimSettings(c *gin.Context) {
	var segments []string
	path := c.Param("path")

	if path == "" {
		segments = nil
	} else {
		segments = RemoveEmptyStrings(strings.Split(path, "/"))
	}

	jsonResult, err := settings.TrimSettings(segments)
	if err != nil {
		c.JSON(http.StatusInternalServerError, jsonResult)
	} else {
		c.JSON(http.StatusOK, jsonResult)
	}
	return
}

func addHeaders(c *gin.Context) {
	c.Header("Cache-Control", "must-revalidate")
	// c.Header("Example-Header", "foo")
	// c.Header("Access-Control-Allow-Origin", "*")
	// c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE")
	// c.Header("Access-Control-Allow-Headers", "X-Custom-Header")
	c.Next()
}

// addTokenToSession checks for a "token" argument, and adds it to the session
// this is easier than passing it around among redirects
func addTokenToSession(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		return
	}
	logger.Info("Saving token insession: %v\n", token)
	session := sessions.Default(c)
	session.Set("token", token)
	err := session.Save()
	if err != nil {
		logger.Info("Error saving session: %s\n", err.Error())
	}
}

// returns true if the setup wizard is completed, or false if not
// if any error occurs it returns true (assumes the wizard is completed)
func isSetupWizardCompleted() bool {
	wizardCompletedJSON, err := settings.GetSettings([]string{"system", "setupWizard", "completed"})
	if err != nil {
		logger.Warn("Failed to read setup wizard completed settings: %v\n", err.Error())
		return true
	}
	if wizardCompletedJSON == nil {
		logger.Warn("Failed to read setup wizard completed settings: %v\n", wizardCompletedJSON)
		return true
	}
	wizardCompletedBool, ok := wizardCompletedJSON.(bool)
	if !ok {
		logger.Warn("Invalid type of setup wizard completed setting: %v %v\n", wizardCompletedJSON, reflect.TypeOf(wizardCompletedJSON))
		return true
	}

	return wizardCompletedBool
}

func ginlogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		logger.Info("GIN: %v %v\n", c.Request.Method, c.Request.RequestURI)
		c.Next()
	}
}

func sysupgradeHandler(c *gin.Context) {
	filename := "/tmp/sysupgrade.img"

	file, _, err := c.Request.FormFile("file")
	if err != nil {
		logger.Warn("Failed to upload file: %s\n", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out, err := os.Create(filename)
	if err != nil {
		logger.Warn("Failed to create %s: %s\n", filename, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	_, err = io.Copy(out, file)
	out.Close()
	if err != nil {
		logger.Warn("Failed to upload image: %s\n", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	logger.Info("Launching sysupgrade...\n")

	err = exec.Command("/sbin/sysupgrade", filename).Run()
	if err != nil {
		logger.Warn("sysupgrade failed: %s\n", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Info("Launching sysupgrade... done\n")

	c.JSON(http.StatusOK, gin.H{"success": true})
	return
}

func upgradeHandler(c *gin.Context) {
	err := exec.Command("/usr/bin/upgrade.sh").Run()
	if err != nil {
		logger.Warn("upgrade failed: %s\n", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Info("Launching upgrade... done\n")

	c.JSON(http.StatusOK, gin.H{"success": true})
	return
}

func releaseDhcp(c *gin.Context) {
	deviceName := c.Param("device")

	logger.Info("Releasing DHCP for device %s...\n", deviceName)

	// var/run/udhcpc-deviceName stores the PID of the DHCP client process with udhcpc on openwrt
	udhcpcPid, err := exec.Command("/bin/cat", fmt.Sprintf("/var/run/udhcpc-%s", deviceName)).CombinedOutput()
	if err != nil {
		// if we cannot find the udhcpc, then this probably isn't an openwrt device
		logger.Warn("Unable to get udhcpc pid: %v - Trying dhclient \n", err)
		err = exec.Command("dhclient", "-r", deviceName).Run()
		if err != nil {
			logger.Warn("Release DHCP failed: %s\n", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		// if we have the PID and no error then try to kill the SIGUSR2 PID (releases IP)
		err := exec.Command("kill", "-SIGUSR2", string(udhcpcPid)).Run()
		if err != nil {
			logger.Warn("Release DHCP failed: %s\n", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
	return
}

func renewDhcp(c *gin.Context) {
	deviceName := c.Param("device")

	logger.Info("Renewing DHCP for device %s...\n", deviceName)

	// var/run/udhcpc-deviceName stores the PID of the DHCP client process with udhcpc on openwrt
	udhcpcPid, err := exec.Command("/bin/cat", fmt.Sprintf("/var/run/udhcpc-%s", deviceName)).CombinedOutput()
	if err != nil {
		// if we cannot find the udhcpc, then this probably isn't an openwrt device
		logger.Warn("Unable to get udhcpc pid: %v - Trying dhclient \n", err)
		err = exec.Command("dhclient", "$wan").Run()
		if err != nil {
			logger.Warn("Renew DHCP failed: %s\n", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		// if we have the PID and no error then try to kill the SIGUSR1 PID (renews IP)
		err := exec.Command("kill", "-SIGUSR1", string(udhcpcPid)).Run()
		if err != nil {
			logger.Warn("Renew DHCP failed: %s\n", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
	return
}
