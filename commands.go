package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/ccatp/antenna-control-unit/datasets"
)

const (
	positionTol              = 1e-4
	speedTol                 = 1e-4
	maxFreeProgramTrackStack = 10000

	azimuthMin      = -180.0
	azimuthMax      = 360.0
	azimuthSpeedMax = 3.0  // [deg/sec]
	azimuthAccelMax = 6.0  // [deg/sec^2]
	azimuthJerkMax  = 12.0 // [deg/sec^3]

	elevationMin      = 0.0
	elevationMax      = 180.0
	elevationSpeedMax = 1.5 // [deg/sec]
	elevationAccelMax = 1.5 // [deg/sec^2]
	elevationJerkMax  = 6.0 // [deg/sec^3]
)

var (
	errAzimuthOutOfRange   = fmt.Errorf("azimuth out of range [%g,%g]", azimuthMin, azimuthMax)
	errElevationOutOfRange = fmt.Errorf("elevation out of range [%g,%g]", elevationMin, elevationMax)
)

func checkAzEl(az, el, vaz, vel float64) error {
	if az < azimuthMin || az > azimuthMax {
		error := fmt.Sprintf("commanded azimuth (%g) out of range [%g,%g]", az, azimuthMin, azimuthMax)
		log.Print(error)
		return fmt.Errorf(error)
	}
	if el < elevationMin || el > elevationMax {
		error := fmt.Sprintf("commanded elevation (%g) out of range [%g,%g]", el, elevationMin, elevationMax)
		log.Print(error)
		return fmt.Errorf(error)
	}
	if math.Abs(vaz) > azimuthSpeedMax {
		error := fmt.Sprintf("commanded azimuth vel (%g) out of range [%g,%g]", vaz, -azimuthSpeedMax, azimuthSpeedMax)
		log.Print(error)
		return fmt.Errorf(error)
	}
	if math.Abs(vel) > elevationSpeedMax {
		error := fmt.Sprintf("commanded elevation vel (%g) out of range [%g,%g]", vel, -elevationSpeedMax, elevationSpeedMax)
		log.Print(error)
		return fmt.Errorf(error)
	}
	return nil
}

type IsDoneFunc func(*datasets.StatusGeneral8100) (bool, error)

type Command interface {
	Check() error
	Start(context.Context, *Telescope) (IsDoneFunc, error)
}

/*
 */

type moveToCmd struct {
	Azimuth   float64
	Elevation float64
}

func (cmd moveToCmd) Check() error {
	return checkAzEl(cmd.Azimuth, cmd.Elevation, 0, 0)
}

func (cmd moveToCmd) Start(ctx context.Context, tel *Telescope) (IsDoneFunc, error) {
	err := tel.MoveTo(cmd.Azimuth, cmd.Elevation)
	isDone := func(rec *datasets.StatusGeneral8100) (bool, error) {
		done := (rec.AzimuthMode == datasets.AzimuthModePreset) &&
			(rec.ElevationMode == datasets.ElevationModePreset) &&
			(math.Abs(rec.AzimuthCurrentPosition-rec.AzimuthCommandedPosition) < positionTol) &&
			(math.Abs(rec.ElevationCurrentPosition-rec.ElevationCommandedPosition) < positionTol) &&
			(math.Abs(rec.AzimuthCurrentVelocity) < speedTol) &&
			(math.Abs(rec.ElevationCurrentVelocity) < speedTol)
		return done, nil
	}
	return isDone, err
}

/*
 */

type azScanCmd struct {
	AzimuthRange   [2]float64 `json:"azimuth_range"`
	Elevation      float64    `json:"elevation"`
	NumScans       int        `json:"num_scans"`
	StartTime      time.Time  `json:"start_time"`
	TurnaroundTime float64    `json:"turnaround_time"`
	Speed          float64    `json:"speed"`
}

func (cmd azScanCmd) Check() error {
	// XXX:TBD
	return nil
}

func startPattern(ctx context.Context, tel *Telescope, pattern ScanPattern) (IsDoneFunc, error) {
	go func() {
		err := tel.UploadScanPattern(ctx, pattern)
		if err != nil {
			log.Print(err)
		}
	}()
	isDone := func(rec *datasets.StatusGeneral8100) (bool, error) {
		// XXX:racy
		done := (rec.QtyOfFreeProgramTrackStackPositions == maxFreeProgramTrackStack-1) && // last point remains on the stack
			(math.Abs(rec.AzimuthCurrentVelocity) < speedTol) &&
			(math.Abs(rec.ElevationCurrentVelocity) < speedTol)
		return done, nil
	}
	return isDone, tel.acu.ModeSet("ProgramTrack")
}

func (cmd azScanCmd) Start(ctx context.Context, tel *Telescope) (IsDoneFunc, error) {
	pattern := NewAzimuthScanPattern(cmd.StartTime, cmd.NumScans, cmd.Elevation, cmd.AzimuthRange, cmd.Speed, time.Duration(cmd.TurnaroundTime*1e9)*time.Nanosecond)
	return startPattern(ctx, tel, pattern)
}

type trackCmd struct {
	StartTime float64 `json:"start_time"`
	StopTime  float64 `json:"stop_time"`
	RA        float64
	Dec       float64
	Coordsys  string
}

func (cmd trackCmd) Check() error {
	switch cmd.Coordsys {
	case "Horizon":
	case "ICRS":
	default:
		return fmt.Errorf("bad coordinate system: %s", cmd.Coordsys)
	}
	if cmd.StopTime < cmd.StartTime {
		return fmt.Errorf("bad times: start=%f, stop=%f", cmd.StartTime, cmd.StopTime)
	}
	return nil
}

func (cmd trackCmd) Start(ctx context.Context, tel *Telescope) (IsDoneFunc, error) {
	pattern, err := NewTrackScanPattern(Unixtime2Time(cmd.StartTime), Unixtime2Time(cmd.StopTime), cmd.RA, cmd.Dec, cmd.Coordsys)
	if err != nil {
		return nil, err
	}
	return startPattern(ctx, tel, pattern)
}

type pathCmd struct {
	Coordsys string
	Points   [][5]float64
}

func (cmd pathCmd) Check() error {
	switch cmd.Coordsys {
	case "Horizon":
	case "ICRS":
	default:
		return fmt.Errorf("bad coordinate system: %s", cmd.Coordsys)
	}

	if len(cmd.Points) == 0 {
		return fmt.Errorf("no points in path")
	}

	// check the times
	for i := 1; i < len(cmd.Points); i++ {
		// ACU ICD 2.0, section 8.9.3:
		// "The minimum time interval between two samples is 0.05 s."
		if cmd.Points[i][0]-cmd.Points[i-1][0] < 0.05 {
			return fmt.Errorf("points are separated by less than 50 ms")
		}
	}

	// check the first 100 coordinates
	pattern := NewPathScanPattern(cmd.Coordsys, cmd.Points)
	iter := pattern.Iterator()
	for i := 0; i < 100; i++ {
		if pattern.Done(iter) {
			break
		}
		var pt datasets.TimePositionTransfer
		err := pattern.Next(iter, &pt)
		if err != nil {
			return err
		}
		err = checkAzEl(pt.AzPosition, pt.ElPosition, pt.AzVelocity, pt.ElVelocity)
		if err != nil {
			return fmt.Errorf("point %d: %w", i, err)
		}
	}

	return nil
}

func (cmd pathCmd) Start(ctx context.Context, tel *Telescope) (IsDoneFunc, error) {
	pattern := NewPathScanPattern(cmd.Coordsys, cmd.Points)
	return startPattern(ctx, tel, pattern)
}
