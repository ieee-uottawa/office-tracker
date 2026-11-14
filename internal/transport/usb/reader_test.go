package usb

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"office/internal/service"
	"strings"
	"testing"
)

func TestUSBOnlyAcceptsProtocolFrames(t *testing.T) {
	calls := 0
	handler := func(_ context.Context, r service.ReaderRequest) service.ReaderResponse {
		calls++
		return service.ReaderResponse{Version: 1, ID: r.ID, Status: 200}
	}
	for _, line := range []string{"Card: 01020304", "--- SIGNOUT ALL ---", "ESP-ROM: boot", "@office:{broken", "@office:" + strings.Repeat("x", maxLine)} {
		require.Empty(t, ProcessLine(context.Background(), line, handler))
	}
	require.Equal(t, 0, calls)
	out := ProcessLine(context.Background(), `@office:{"v":1,"id":"boot-1","op":"scan","data":{"uid":"01020304"}}`, handler)
	require.Equal(t, 1, calls)
	require.Contains(t, string(out), `"id":"boot-1"`)
}

type fragmentedPort struct {
	input  *bytes.Buffer
	output bytes.Buffer
}

func (p *fragmentedPort) Read(b []byte) (int, error) {
	if len(b) > 3 {
		b = b[:3]
	}
	return p.input.Read(b)
}
func (p *fragmentedPort) Write(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return p.output.Write(b)
}

func TestUSBFramingHandlesFragmentsAndOverflow(t *testing.T) {
	p := &fragmentedPort{input: bytes.NewBufferString(strings.Repeat("x", maxLine+10) + "\nCard: 01020304\n@office:{\"v\":1,\"id\":\"test-1\",\"op\":\"heartbeat\",\"data\":{}}\r\n")}
	calls := 0
	err := serve(context.Background(), p, func(_ context.Context, r service.ReaderRequest) service.ReaderResponse {
		calls++
		return service.ReaderResponse{Version: 1, ID: r.ID, Status: 200}
	})
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 1, calls)
	require.Equal(t, "@office:{\"v\":1,\"id\":\"test-1\",\"status\":200}\n", p.output.String())
}
