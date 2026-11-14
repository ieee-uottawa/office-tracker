package relay_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"office/internal/database"
	"office/internal/repository"
	"office/internal/service"
	transport "office/internal/transport/http"
	"office/internal/transport/relay"
	"path/filepath"
	"testing"
	"time"
)

func TestRelayAuthenticatedReplayAndFailure(t *testing.T) {
	db := database.Open(filepath.Join(t.TempDir(), "office.db"))
	defer db.Close()
	require.NoError(t, database.Migrate(db))
	_, err := db.Exec("INSERT INTO users(id,name,rfid_uid) VALUES(1,'Relay test','01020304')")
	require.NoError(t, err)
	att := service.NewAttendanceService(&repository.UserRepo{DB: db}, &repository.SessionRepo{DB: db})
	server := transport.New("0", att, nil, nil, nil, nil, nil, transport.MiddlewareConfig{APIKeyEnabled: true, APIKey: "test-key"})
	server.Reader = &service.USBReaderService{DB: db, Attendance: att, Health: service.NewESPHealthService(time.Minute)}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	client := &relay.Client{URL: ts.URL + "/api/reader/request", APIKey: "wrong", HTTP: ts.Client()}
	req := service.ReaderRequest{Version: 1, ID: "relay-1", Operation: "scan", Data: json.RawMessage(`{"uid":"01020304"}`)}
	require.Equal(t, 503, client.Handle(context.Background(), req).Status)
	client.APIKey = "test-key"
	first := client.Handle(context.Background(), req)
	require.Equal(t, 200, first.Status)
	require.Equal(t, "check-in", first.Action)
	// Treat the first reply as lost and resend the original frame.
	require.Equal(t, first, client.Handle(context.Background(), req))
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sessions").Scan(&count))
	require.Equal(t, 1, count)
	req.Data = json.RawMessage(`{"uid":"05060708"}`)
	require.Equal(t, 409, client.Handle(context.Background(), req).Status)
	req.Operation = "heartbeat"
	req.ID = "health-1"
	req.Data = json.RawMessage(`{"device_id":"reader","uptime_seconds":1}`)
	require.Equal(t, 200, client.Handle(context.Background(), req).Status)
	require.Len(t, server.Reader.Health.GetAll(), 1)
	ts.Close()
	require.Equal(t, 503, client.Handle(context.Background(), req).Status)
}

func TestRelayRejectsMismatchedReply(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"v":1,"id":"different","status":200}`))
	}))
	defer ts.Close()
	client := relay.Client{URL: ts.URL, HTTP: ts.Client()}
	require.Equal(t, 503, client.Handle(context.Background(), service.ReaderRequest{Version: 1, ID: "original"}).Status)
}
