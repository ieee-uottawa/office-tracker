// Package usb carries acknowledged reader requests over the Pi's USB serial port.
package usb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"go.bug.st/serial"
	"office/internal/logging"
	"office/internal/service"
)

const Prefix = "@office:"
const maxLine = 2048

type Handler func(context.Context, service.ReaderRequest) service.ReaderResponse

// ProcessLine ignores diagnostics and boot messages. Only framed JSON can mutate attendance.
func ProcessLine(ctx context.Context, line string, handle Handler) []byte {
	if !strings.HasPrefix(line, Prefix) || len(line) > maxLine {
		return nil
	}
	var req service.ReaderRequest
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, Prefix)), &req); err != nil {
		return nil
	}
	reply := handle(ctx, req)
	data, err := json.Marshal(reply)
	if err != nil {
		return nil
	}
	return append(append([]byte(Prefix), data...), '\n')
}

func Run(ctx context.Context, path string, handle Handler) {
	log := logging.Component("usb")
	for ctx.Err() == nil {
		port, err := serial.Open(path, &serial.Mode{BaudRate: 115200, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit})
		if err == nil {
			// Release the ESP32 reset/boot signals. Virtual serial ports do not
			// implement modem-control ioctls, so ENOTTY is expected there.
			for _, set := range []func(bool) error{port.SetDTR, port.SetRTS} {
				if controlErr := set(false); controlErr != nil && !errors.Is(controlErr, syscall.ENOTTY) {
					err = controlErr
					break
				}
			}
			if err == nil {
				err = port.SetReadTimeout(250 * time.Millisecond)
			}
			if err == nil {
				log.Info("USB reader connected", "port", path)
				err = serve(ctx, port, handle)
			}
			port.Close()
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("USB reader disconnected; retrying", "port", path, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func serve(ctx context.Context, port io.ReadWriter, handle Handler) error {
	buf := make([]byte, 256)
	line := make([]byte, 0, 256)
	overflow := false
	for ctx.Err() == nil {
		n, err := port.Read(buf)
		if err != nil {
			return err
		}
		for _, b := range buf[:n] {
			if b == '\r' {
				continue
			}
			if b != '\n' {
				if len(line) < maxLine {
					line = append(line, b)
				} else {
					overflow = true
				}
				continue
			}
			if !overflow {
				if reply := ProcessLine(ctx, string(line), handle); reply != nil {
					for len(reply) > 0 {
						written, err := port.Write(reply)
						if err != nil {
							return err
						}
						if written == 0 {
							return fmt.Errorf("USB short write")
						}
						reply = reply[written:]
					}
				}
			}
			line = line[:0]
			overflow = false
		}
	}
	return ctx.Err()
}
