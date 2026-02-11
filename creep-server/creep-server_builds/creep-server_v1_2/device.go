package main

import (
	"bufio"
	"github.com/tarm/serial"
)

type Device struct {
	SerialPort *serial.Port
}

func (d *Device) ReadDeviceID() (string, error) {
	return d.MakeCommand("ID")
}

func (d *Device) MakeCommand(cmd string) (res string, err error) {
	_, err = d.SerialPort.Write([]byte(cmd + "\n"))
	if err != nil {
		return
	}

	reader := bufio.NewReader(d.SerialPort)
	res, err = reader.ReadString(10)
	if err == nil && len(res) > 0 {
		res = res[:len(res)-1]
	}
	return
}
