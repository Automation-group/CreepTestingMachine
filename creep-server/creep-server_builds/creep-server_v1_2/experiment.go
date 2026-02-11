package main

import (
	"sync"
)

type Experiment struct {
	sync.RWMutex
	CurrentForceCode    int
	ZeroForce           float64
	ZeroPosition        float64
	CurrentPositionCode int
	CurrentForce        float64
	CurrentPosition     float64
	Temperature1        int
	Temperature2        int
	Temperature3        int
	DefCoeff            float64 `json:"DefCoeff"`
}
