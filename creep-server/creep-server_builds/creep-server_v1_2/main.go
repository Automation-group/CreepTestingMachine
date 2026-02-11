package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tarm/serial"
	"github.com/tidwall/redcon"
)

// setupConfig initializes viper to manage application configuration.
func setupConfig() {
	viper.SetConfigName("config")              // name of config file (without extension)
	viper.SetConfigType("yaml")                // REQUIRED if the config file does not have the extension in the name
	viper.AddConfigPath(".")                   // look for config in the working directory
	viper.AddConfigPath("/etc/creep-server/")  // path to look for the config file in
	viper.AddConfigPath("$HOME/.creep-server") // call multiple times to add many search paths

	// Set defaults
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.format", "text")
	viper.SetDefault("server.address", ":6380")
	viper.SetDefault("server.password", "dddddddddd")
	viper.SetDefault("experiment.deformation_coeff", 0.439822972)
	viper.SetDefault("experiment.data_file", "experiment.json")
	viper.SetDefault("experiment.force_coefficients.a", 2016.3)
	viper.SetDefault("experiment.force_coefficients.b", 0.059543)
	viper.SetDefault("experiment.force_coefficients.c", 73.4)
	viper.SetDefault("serial.baud", 115200)
	viper.SetDefault("devices.driver.id", "ID=201602-creep-driver")
	viper.SetDefault("devices.indicator.id", "ID=indikator16_20160209")
	viper.SetDefault("devices.forcesensor.id", "ID2f8771d5ebf55e0983210304c6d5197e")
	viper.SetDefault("devices.metakon.id", "ID=metakon-commander-201603")

	// Environment variable binding
	viper.SetEnvPrefix("CREEP")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Read in config file
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found; ignore error if desired
			slog.Warn("Config file not found, using defaults and environment variables.")
		} else {
			// Config file was found but another error was produced
			slog.Error("Error reading config file", "error", err)
		}
	} else {
		slog.Info("Using config file", "path", viper.ConfigFileUsed())
	}
}

// setupLogger configures slog with structured logging based on viper config.
func setupLogger() {
	var level slog.Level
	switch strings.ToLower(viper.GetString("log.level")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var handler slog.Handler
	if viper.GetString("log.format") == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		})
	}
	slog.SetDefault(slog.New(handler))
}

func findSerialPorts() (res []string) {
	res = make([]string, 0)
	infos, err := ioutil.ReadDir("/dev")
	if err != nil {
		return
	}

	portNameReg := regexp.MustCompile(`tty(USB|ACM)\d+`)

	for _, info := range infos {
		name := info.Name()
		if !info.IsDir() && portNameReg.MatchString(name) {
			res = append(res, "/dev/"+name)
		}
	}

	return
}

var addr = ":6380"

type emptyStruct struct{}

var isSerialPortUsingMutex sync.RWMutex
var isSerialPortUsing = make(map[string]emptyStruct)

var experiment = Experiment{}

type Command struct {
	Cmd      string
	Response chan string
}

var forceSensorCommandChannel = make(chan Command, 10)
var driverCommandChannel = make(chan Command, 10)
var metakonCommandChannel = make(chan Command, 10)

func ProcessForceSensor(d *Device) {
	slog.Info("Starting force sensor processing")
	re := regexp.MustCompile(`GM(-?\d+)`)
	for {
		slog.Debug("Sending GM command to force sensor")
		res, err := d.MakeCommand("GM")
		if err != nil {
			slog.Error("ProcessForceSensor error", "error", err)
			break
		}
		slog.Debug("Received response from force sensor", "response", res)
		matches := re.FindStringSubmatch(res)
		if len(matches) > 1 {
			code, err := strconv.Atoi(matches[1])
			if err != nil {
				slog.Error("Failed to convert force code to int", "value", matches[1], "error", err)
			} else {
				slog.Debug("Parsed force code", "code", code)
				experiment.Lock()
				experiment.CurrentForceCode = code
				coeffA := viper.GetFloat64("experiment.force_coefficients.a")
				coeffB := viper.GetFloat64("experiment.force_coefficients.b")
				coeffC := viper.GetFloat64("experiment.force_coefficients.c")
				experiment.CurrentForce = coeffA - coeffB*float64(code) - coeffC - experiment.ZeroForce // Н
				slog.Debug("Updated experiment force values", "CurrentForceCode", experiment.CurrentForceCode, "CurrentForce", experiment.CurrentForce)
				experiment.Unlock()
			}
		} else {
			slog.Warn("No force code found in response", "response", res)
			break
		}
		select {
		case cmd := <-forceSensorCommandChannel:
			slog.Info("Received command for force sensor", "command", cmd.Cmd)
			res, err := d.MakeCommand(cmd.Cmd)
			if err != nil {
				slog.Error("ProcessForceSensor command error", "command", cmd.Cmd, "error", err)
			} else {
				slog.Debug("Force sensor command response", "command", cmd.Cmd, "response", res)
			}
			cmd.Response <- res
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func ProcessDriver(d *Device) {
	slog.Info("Starting driver processing")
	re := regexp.MustCompile(`GP(-?\d+)`)
	for {
		slog.Debug("Sending GP command to driver")
		res, err := d.MakeCommand("GP")
		if err != nil {
			slog.Error("ProcessDriver error", "error", err)
			break
		}
		slog.Debug("Received response from driver", "response", res)
		matches := re.FindStringSubmatch(res)
		if len(matches) > 1 {
			code, err := strconv.Atoi(matches[1])
			if err != nil {
				slog.Error("Failed to convert position code to int", "value", matches[1], "error", err)
			} else {
				slog.Debug("Parsed position code", "code", code)
				experiment.Lock()
				experiment.CurrentPositionCode = code
				experiment.CurrentPosition = float64(code)*experiment.DefCoeff - experiment.ZeroPosition // мкм
				slog.Debug("Updated experiment position values", "CurrentPositionCode", experiment.CurrentPositionCode, "CurrentPosition", experiment.CurrentPosition)
				experiment.Unlock()
			}
		} else {
			slog.Warn("No position code found in response", "response", res)
			break
		}

		select {
		case cmd := <-driverCommandChannel:
			slog.Info("Received command for driver", "command", cmd.Cmd)
			res, err := d.MakeCommand(cmd.Cmd)
			if err != nil {
				slog.Error("ProcessDriver command error", "command", cmd.Cmd, "error", err)
			} else {
				slog.Debug("Driver command response", "command", cmd.Cmd, "response", res)
			}
			cmd.Response <- res
		default:
			time.Sleep(200 * time.Millisecond)
		}

	}
}

func ProcessMetakon(d *Device) {
	slog.Info("Starting metakon processing")
	re := regexp.MustCompile(`CT\d+=(-?\d+)`)
	for {
		slog.Debug("Sending CT0 command to metakon")
		res, err := d.MakeCommand("CT0")
		if err != nil {
			slog.Error("ProcessMetakon error on CT0", "error", err)
			break
		}
		slog.Debug("Received response from metakon CT0", "response", res)
		matches := re.FindStringSubmatch(res)
		if len(matches) > 1 {
			code, err := strconv.Atoi(matches[1])
			if err != nil {
				slog.Error("Failed to convert CT0 code to int", "value", matches[1], "error", err)
			} else {
				slog.Debug("Parsed CT0 temperature code", "code", code)
				experiment.Lock()
				experiment.Temperature1 = code
				slog.Debug("Updated experiment.Temperature1", "Temperature1", experiment.Temperature1)
				experiment.Unlock()
			}
		} else {
			slog.Error("Metakon read CT0 error: no match", "result", res)
		}

		slog.Debug("Sending CT1 command to metakon")
		res, err = d.MakeCommand("CT1")
		if err != nil {
			slog.Error("ProcessMetakon error on CT1", "error", err)
			break
		}
		slog.Debug("Received response from metakon CT1", "response", res)
		matches = re.FindStringSubmatch(res)
		if len(matches) > 1 {
			code, err := strconv.Atoi(matches[1])
			if err != nil {
				slog.Error("Failed to convert CT1 code to int", "value", matches[1], "error", err)
			} else {
				slog.Debug("Parsed CT1 temperature code", "code", code)
				experiment.Lock()
				experiment.Temperature2 = code
				slog.Debug("Updated experiment.Temperature2", "Temperature2", experiment.Temperature2)
				experiment.Unlock()
			}
		} else {
			slog.Error("Metakon read CT1 error: no match", "result", res)
		}

		slog.Debug("Sending CT2 command to metakon")
		res, err = d.MakeCommand("CT2")
		if err != nil {
			slog.Error("ProcessMetakon error on CT2", "error", err)
			break
		}
		slog.Debug("Received response from metakon CT2", "response", res)
		matches = re.FindStringSubmatch(res)
		if len(matches) > 1 {
			code, err := strconv.Atoi(matches[1])
			if err != nil {
				slog.Error("Failed to convert CT2 code to int", "value", matches[1], "error", err)
			} else {
				slog.Debug("Parsed CT2 temperature code", "code", code)
				experiment.Lock()
				experiment.Temperature3 = code
				slog.Debug("Updated experiment.Temperature3", "Temperature3", experiment.Temperature3)
				experiment.Unlock()
			}
		} else {
			slog.Error("Metakon read CT2 error: no match", "result", res)
		}

		select {
		case cmd := <-metakonCommandChannel:
			slog.Info("Received command for metakon", "command", cmd.Cmd)
			res, err := d.MakeCommand(cmd.Cmd)
			if err != nil {
				slog.Error("ProcessMetakon command error", "command", cmd.Cmd, "error", err)
			} else {
				slog.Debug("Metakon command response", "command", cmd.Cmd, "response", res)
			}
			cmd.Response <- res
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func ProcessIndikator(d *Device) {
	slog.Info("Starting indicator processing")
	for {
		experiment.RLock()
		force := experiment.CurrentForce
		pos := experiment.CurrentPosition
		experiment.RUnlock()

		slog.Debug("Preparing to send force to indicator", "force", force)
		value := strconv.FormatFloat(force, 'f', 1, 64)
		if len(value) < 8 {
			value = strings.Repeat(" ", 7-len(value)) + value
		}
		cmd1 := "P1" + value
		slog.Debug("Sending command to indicator", "command", cmd1)
		_, err := d.MakeCommand(cmd1)
		if err != nil {
			slog.Error("ProcessIndikator error sending force", "command", cmd1, "error", err)
			break
		} else {
			slog.Debug("Successfully sent force to indicator", "command", cmd1)
		}

		slog.Debug("Preparing to send position to indicator", "position", pos)
		value = strconv.FormatFloat(pos, 'f', 1, 64)
		if len(value) < 8 {
			value = strings.Repeat(" ", 7-len(value)) + value
		}
		cmd2 := "P2" + value
		slog.Debug("Sending command to indicator", "command", cmd2)
		_, err = d.MakeCommand(cmd2)
		if err != nil {
			slog.Error("ProcessIndikator error sending position", "command", cmd2, "error", err)
			break
		} else {
			slog.Debug("Successfully sent position to indicator", "command", cmd2)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func FindAndConnectSerialPorts() {
	slog.Info("Scanning for serial ports")
	baudRate := viper.GetInt("serial.baud")

	for _, port := range findSerialPorts() {
		slog.Debug("Checking port", "port", port)
		isSerialPortUsingMutex.RLock()
		_, ok := isSerialPortUsing[port]
		isSerialPortUsingMutex.RUnlock()
		if ok {
			slog.Debug("Port already in use, skipping", "port", port)
			continue
		}

		c := &serial.Config{Name: port, Baud: baudRate, ReadTimeout: time.Second}
		slog.Info("Attempting to open serial port", "port", port, "baud", c.Baud)
		s, err := serial.OpenPort(c)
		if err != nil {
			slog.Error("Failed to open serial port", "port", port, "error", err)
			continue
		}

		device := Device{SerialPort: s}
		slog.Debug("Reading device ID", "port", port)
		r, err := device.ReadDeviceID()
		if err == nil && strings.Contains(r, "ID") {
			slog.Info("Device ID read successfully", "port", port, "device_id", r)
			isSerialPortUsingMutex.Lock()
			isSerialPortUsing[port] = emptyStruct{}
			isSerialPortUsingMutex.Unlock()

			switch r {
			case viper.GetString("devices.driver.id"):
				slog.Info("Connected to driver device", "port", port, "device_id", r)
				go func(d *Device, port string) {
					ProcessDriver(d)
					slog.Info("Driver processing ended", "port", port)
					isSerialPortUsingMutex.Lock()
					delete(isSerialPortUsing, port)
					isSerialPortUsingMutex.Unlock()
					d.SerialPort.Close()
					slog.Info("Driver device disconnected", "port", port)
				}(&device, port)
			case viper.GetString("devices.indicator.id"):
				slog.Info("Connected to indicator device", "port", port, "device_id", r)
				go func(d *Device, port string) {
					ProcessIndikator(d)
					slog.Info("Indicator processing ended", "port", port)
					isSerialPortUsingMutex.Lock()
					delete(isSerialPortUsing, port)
					isSerialPortUsingMutex.Unlock()
					d.SerialPort.Close()
					slog.Info("Indicator device disconnected", "port", port)
				}(&device, port)
			case viper.GetString("devices.forcesensor.id"):
				slog.Info("Connected to force sensor device", "port", port, "device_id", r)
				go func(d *Device, port string) {
					ProcessForceSensor(d)
					slog.Info("Force sensor processing ended", "port", port)
					isSerialPortUsingMutex.Lock()
					delete(isSerialPortUsing, port)
					isSerialPortUsingMutex.Unlock()
					d.SerialPort.Close()
					slog.Info("Force sensor device disconnected", "port", port)
				}(&device, port)
			case viper.GetString("devices.metakon.id"):
				slog.Info("Connected to metakon device", "port", port, "device_id", r)
				go func(d *Device, port string) {
					ProcessMetakon(d)
					slog.Info("Metakon processing ended", "port", port)
					isSerialPortUsingMutex.Lock()
					delete(isSerialPortUsing, port)
					isSerialPortUsingMutex.Unlock()
					d.SerialPort.Close()
					slog.Info("Metakon device disconnected", "port", port)
				}(&device, port)
			default:
				slog.Warn("Unknown device ID", "port", port, "device_id", r)
				isSerialPortUsingMutex.Lock()
				delete(isSerialPortUsing, port)
				isSerialPortUsingMutex.Unlock()
				device.SerialPort.Close()
			}

			slog.Debug("Port scan result", "port", port, "result", r, "error", err)
		} else {
			if err != nil {
				slog.Warn("Failed to read device ID", "port", port, "error", err)
			} else {
				slog.Warn("Device ID not recognized", "port", port, "response", r)
			}
			slog.Debug("Closing unused port", "port", port)
			device.SerialPort.Close()
		}
	}
	slog.Info("Serial port scan complete")
}

type Ctx struct {
	isLogged bool
}

func loadExperiment() {
	dataFile := viper.GetString("experiment.data_file")
	data, err := ioutil.ReadFile(dataFile)
	if err == nil {
		json.Unmarshal(data, &experiment)
		slog.Info("Loaded experiment data", "file", dataFile)
	} else {
		if !os.IsNotExist(err) {
			slog.Error("Error loading experiment data", "file", dataFile, "error", err)
		}
	}
}

func saveExperiment() {
	dataFile := viper.GetString("experiment.data_file")
	data, err := json.Marshal(&experiment)
	if err == nil {
		ioutil.WriteFile(dataFile, data, 0644)
		slog.Info("Saved experiment data", "file", dataFile)
	} else {
		slog.Error("Error saving experiment data", "file", dataFile, "error", err)
	}
}

func main() {
	// Initialize configuration and logging first
	setupConfig()
	setupLogger()

	// Initialize experiment with value from config
	experiment.DefCoeff = viper.GetFloat64("experiment.deformation_coeff")

	slog.Info("Starting creep server", "version", "1.0")
	loadExperiment()

	go func() {
		for {
			FindAndConnectSerialPorts()
			time.Sleep(5 * time.Second)
		}
	}()

	var mu sync.RWMutex
	var items = make(map[string][]byte)

	serverAddr := viper.GetString("server.address")
	serverPassword := viper.GetString("server.password")

	slog.Info("Starting server", "address", serverAddr)
	err := redcon.ListenAndServe(serverAddr,
		func(conn redcon.Conn, cmd redcon.Command) {
			cmdName := strings.ToLower(string(cmd.Args[0]))
			ctx := conn.Context().(*Ctx)
			if ctx == nil {
				conn.Close()
				return
			}

			if cmdName != "auth" && !ctx.isLogged {
				conn.WriteError("ERR not authorized")
				conn.Close()
				return
			}

			switch cmdName {
			default:
				conn.WriteError("ERR unknown command '" + string(cmd.Args[0]) + "'")
			case "auth":
				if len(cmd.Args) != 2 {
					conn.WriteError("ERR wrong number of arguments for '" + string(cmd.Args[0]) + "' command")
					return
				}
				if string(cmd.Args[1]) == serverPassword {
					ctx.isLogged = true
					conn.WriteString("OK")
				} else {
					conn.WriteError("ERR wrong password")
					return
				}
			case "ping":
				conn.WriteString("PONG")
			case "quit":
				conn.WriteString("OK")
				conn.Close()
			case "getset":
				if len(cmd.Args) != 3 {
					conn.WriteError("ERR wrong number of arguments for '" + string(cmd.Args[0]) + "' command")
					return
				}

				key := string(cmd.Args[1])
				cmdStr := string(cmd.Args[2])

				response := make(chan string)
				result := ""
				switch key {
				case "acc":
					forceSensorCommandChannel <- Command{Cmd: cmdStr, Response: response}
					result = <-response
				case "metakon":
					metakonCommandChannel <- Command{Cmd: cmdStr, Response: response}
					result = <-response
				case "stepmotor":
					driverCommandChannel <- Command{Cmd: cmdStr, Response: response}
					result = <-response
				case "deformation_coeff":
					v, err := strconv.ParseFloat(cmdStr, 64)
					if err == nil {
						experiment.Lock()
						experiment.DefCoeff = v
						experiment.Unlock()
						saveExperiment()
					}
					result = strconv.FormatFloat(experiment.DefCoeff, 'f', -1, 64)
				}

				conn.WriteBulkString(result)

			case "set":
				if len(cmd.Args) != 3 {
					conn.WriteError("ERR wrong number of arguments for '" + string(cmd.Args[0]) + "' command")
					return
				}
				key := string(cmd.Args[1])
				value := string(cmd.Args[2])
				mu.Lock()
				items[string(cmd.Args[1])] = cmd.Args[2]
				mu.Unlock()

				switch key {
				case "force zero":
					experiment.Lock()
					experiment.ZeroForce = experiment.CurrentForce + experiment.ZeroForce
					experiment.Unlock()
				case "position zero":
					experiment.Lock()
					experiment.ZeroPosition = experiment.CurrentPosition + experiment.ZeroPosition
					experiment.Unlock()
				case "deformation_coeff":
					v, err := strconv.ParseFloat(value, 64)
					if err == nil {
						experiment.Lock()
						experiment.DefCoeff = v
						experiment.Unlock()
						saveExperiment()
					}
				case "force set point":
					setpoint, err := strconv.ParseFloat(value, 54)
					if err == nil {
						coeffA := viper.GetFloat64("experiment.force_coefficients.a")
						coeffB := viper.GetFloat64("experiment.force_coefficients.b")
						coeffC := viper.GetFloat64("experiment.force_coefficients.c")
						spCode := int((coeffA - setpoint - coeffC - experiment.ZeroForce) / coeffB)
						response := make(chan string)
						cmdToDevice := fmt.Sprintf("SP%d", spCode)

						forceSensorCommandChannel <- Command{Cmd: cmdToDevice, Response: response}
						<-response
					}
				}

				conn.WriteString("OK")
			case "get":
				if len(cmd.Args) != 2 {
					conn.WriteError("ERR wrong number of arguments for '" + string(cmd.Args[0]) + "' command")
					return
				}

				key := string(cmd.Args[1])
				result := ""
				experiment.RLock()
				switch key {
				case "temperature1":
					result = strconv.Itoa(experiment.Temperature1)
				case "temperature2":
					result = strconv.Itoa(experiment.Temperature2)
				case "temperature3":
					result = strconv.Itoa(experiment.Temperature3)
				case "ports":
					result = strings.Join(findSerialPorts(), ",")
				case "force":
					result = strconv.FormatFloat(experiment.CurrentForce, 'f', 2, 64)
				case "position":
					result = strconv.FormatFloat(experiment.CurrentPosition, 'f', 2, 64)
				case "deformation_coeff":
					result = strconv.FormatFloat(experiment.DefCoeff, 'f', -1, 64)
				}
				experiment.RUnlock()

				conn.WriteBulkString(result)

			case "del":
				if len(cmd.Args) != 2 {
					conn.WriteError("ERR wrong number of arguments for '" + string(cmd.Args[0]) + "' command")
					return
				}
				mu.Lock()
				_, ok := items[string(cmd.Args[1])]
				delete(items, string(cmd.Args[1]))
				mu.Unlock()
				if !ok {
					conn.WriteInt(0)
				} else {
					conn.WriteInt(1)
				}
			}
		},
		func(conn redcon.Conn) bool {
			ctx := &Ctx{}
			ctx.isLogged = false
			conn.SetContext(ctx)
			return true
		},
		func(conn redcon.Conn, err error) {
			// this is called when the connection has been closed
		},
	)
	if err != nil {
		slog.Error("Fatal server error", "error", err)
		panic(err)
	}
}
