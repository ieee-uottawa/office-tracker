// reader-relay runs on the USB-attached Pi; all business logic runs on the server.
package main

import (
	"context"
	"net/http"
	"office/internal/logging"
	"office/internal/transport/relay"
	"office/internal/transport/usb"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logging.Setup()
	path, endpoint, key := os.Getenv("USB_READER_PORT"), os.Getenv("READER_SERVER_URL"), os.Getenv("API_KEY")
	if path == "" || endpoint == "" || key == "" {
		panic("USB_READER_PORT, READER_SERVER_URL and API_KEY are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &relay.Client{URL: endpoint, APIKey: key, HTTP: &http.Client{}}
	usb.Run(ctx, path, client.Handle)
}
