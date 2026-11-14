package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"office/internal/database"
	"office/internal/repository"
)

func readerFixture(t *testing.T) *USBReaderService {
	t.Helper()
	db := database.Open(filepath.Join(t.TempDir(), "office.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	_, err := db.Exec("INSERT INTO users(id,name,rfid_uid,jingle) VALUES(1,'Test One','01020304','mario'),(2,'Test Two','05060708','jana')")
	require.NoError(t, err)
	return &USBReaderService{DB: db, Attendance: NewAttendanceService(&repository.UserRepo{DB: db}, &repository.SessionRepo{DB: db}), Health: NewESPHealthService(time.Minute)}
}

func readerScan(id, uid string) ReaderRequest {
	return ReaderRequest{Version: 1, ID: id, Operation: "scan", Data: json.RawMessage(`{"uid":"` + uid + `"}`)}
}
func openCount(t *testing.T, s *USBReaderService) int {
	t.Helper()
	var n int
	require.NoError(t, s.DB.QueryRow("SELECT count(*) FROM sessions WHERE check_out IS NULL").Scan(&n))
	return n
}

func TestUSBReceiptSurvivesRestart(t *testing.T) {
	s := readerFixture(t)
	ctx := context.Background()
	req := readerScan("boot-1", "01020304")
	first := s.Handle(ctx, req)
	require.Equal(t, 200, first.Status)
	require.Equal(t, "check-in", first.Action)
	require.Equal(t, "mario", first.Jingle)
	// Recreate the in-memory services: the durable receipt must still win.
	s.Attendance = NewAttendanceService(&repository.UserRepo{DB: s.DB}, &repository.SessionRepo{DB: s.DB})
	require.Equal(t, first, s.Handle(ctx, req))
	require.Equal(t, 1, openCount(t, s))
	next := s.Handle(ctx, readerScan("boot-2", "01020304"))
	require.Equal(t, "check-out", next.Action)
	require.Equal(t, 0, openCount(t, s))
	conflict := s.Handle(ctx, readerScan("boot-1", "05060708"))
	require.Equal(t, 409, conflict.Status)
	require.Equal(t, 0, openCount(t, s))
}

func TestUSBConcurrentRetryOnlyCreatesOneSession(t *testing.T) {
	s := readerFixture(t)
	var wg sync.WaitGroup
	responses := make(chan ReaderResponse, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- s.Handle(context.Background(), readerScan("retry-1", "01020304"))
		}()
	}
	wg.Wait()
	close(responses)
	for r := range responses {
		require.Equal(t, 200, r.Status)
		require.Equal(t, "check-in", r.Action)
	}
	require.Equal(t, 1, openCount(t, s))
	var n int
	require.NoError(t, s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&n))
	require.Equal(t, 1, n)
}

func TestUSBReceiptFailureRollsBackAttendance(t *testing.T) {
	s := readerFixture(t)
	_, err := s.DB.Exec("CREATE TRIGGER fail_receipt BEFORE INSERT ON reader_requests BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END")
	require.NoError(t, err)
	r := s.Handle(context.Background(), readerScan("atomic-1", "01020304"))
	require.Equal(t, 500, r.Status)
	require.Equal(t, 0, openCount(t, s))
	_, err = s.DB.Exec("DROP TRIGGER fail_receipt")
	require.NoError(t, err)
	r = s.Handle(context.Background(), readerScan("atomic-1", "01020304"))
	require.Equal(t, 200, r.Status)
	require.Equal(t, "check-in", r.Action)
}

func TestUSBCheckoutAllRetryDoesNotCloseNewArrivals(t *testing.T) {
	s := readerFixture(t)
	ctx := context.Background()
	require.Equal(t, 200, s.Handle(ctx, readerScan("scan-1", "01020304")).Status)
	req := ReaderRequest{Version: 1, ID: "all-1", Operation: "checkout_all", Data: json.RawMessage(`{}`)}
	first := s.Handle(ctx, req)
	require.Equal(t, int64(1), first.Count)
	require.Equal(t, 200, s.Handle(ctx, readerScan("scan-2", "05060708")).Status)
	require.Equal(t, first, s.Handle(ctx, req))
	require.Equal(t, 1, openCount(t, s))
}

func TestUSBUnknownCardAndHeartbeat(t *testing.T) {
	s := readerFixture(t)
	ctx := context.Background()
	require.Equal(t, 404, s.Handle(ctx, readerScan("unknown-1", "AABBCCDD")).Status)
	require.Equal(t, 0, openCount(t, s))
	require.Equal(t, 400, s.Handle(ctx, readerScan("invalid-1", "bogus")).Status)
	r := s.Handle(ctx, ReaderRequest{Version: 1, ID: "ping-1", Operation: "heartbeat", Data: json.RawMessage(`{"device_id":"reader","uptime_seconds":3,"firmware_version":"1.1.0-usb"}`)})
	require.Equal(t, 200, r.Status)
	require.Equal(t, "usb", s.Health.GetAll()[0].Transport)
	require.True(t, s.Health.IsFresh(s.Health.GetAll()[0]))
}
