package service

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"office/internal/domain"
)

type ReaderRequest struct {
	Version   int             `json:"v"`
	ID        string          `json:"id"`
	Operation string          `json:"op"`
	Data      json.RawMessage `json:"data"`
}

type ReaderResponse struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
	Status  int    `json:"status"`
	Action  string `json:"action,omitempty"`
	User    string `json:"user,omitempty"`
	Jingle  string `json:"jingle,omitempty"`
	Error   string `json:"error,omitempty"`
	Count   int64  `json:"count,omitempty"`
}

type USBReaderService struct {
	DB         *sql.DB
	Attendance *AttendanceService
	Health     *ESPHealthService
}

var readerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// Handle commits attendance and the response in one transaction. Replaying a
// request, even after restarting the Pi, returns the original response.
func (s *USBReaderService) Handle(ctx context.Context, req ReaderRequest) ReaderResponse {
	reply := ReaderResponse{Version: 1, ID: req.ID, Status: 400}
	fail := func(status int, msg string) ReaderResponse { reply.Status = status; reply.Error = msg; return reply }
	if req.Version != 1 || !readerIDPattern.MatchString(req.ID) {
		return fail(400, "invalid protocol or request ID")
	}
	if req.Operation == "heartbeat" {
		var status domain.ESPHealthStatus
		if err := json.Unmarshal(req.Data, &status); err != nil || status.DeviceID == "" || status.UptimeSeconds < 0 || status.FreeHeapBytes < 0 {
			return fail(400, "invalid heartbeat")
		}
		status.Transport = "usb"
		status.WiFiConnected = false
		status.IP = ""
		s.Health.Update(status)
		reply.Status = 200
		reply.Action = "ready"
		return reply
	}
	if req.Operation != "scan" && req.Operation != "checkout_all" {
		return fail(400, "unknown operation")
	}
	var data struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return fail(400, "invalid payload")
	}
	data.UID = strings.ToUpper(strings.TrimSpace(data.UID))
	if req.Operation == "scan" {
		uid, err := hex.DecodeString(data.UID)
		if err != nil || (len(uid) != 4 && len(uid) != 7 && len(uid) != 10) {
			return fail(400, "invalid card UID")
		}
	}
	payload := req.Operation + ":" + data.UID
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return fail(503, "database unavailable")
	}
	defer conn.Close()
	// busy_timeout is connection-local in SQLite.
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout = 4000"); err != nil {
		return fail(503, "database unavailable")
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fail(503, "database busy")
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var savedPayload, savedResponse string
	err = conn.QueryRowContext(ctx, "SELECT payload,response FROM reader_requests WHERE request_id=?", req.ID).Scan(&savedPayload, &savedResponse)
	if err == nil {
		if savedPayload != payload {
			return fail(409, "request ID already used for another operation")
		}
		if json.Unmarshal([]byte(savedResponse), &reply) != nil {
			return fail(500, "invalid saved response")
		}
		return reply
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fail(500, "database read failed")
	}
	reply.Status = 200
	var userID int64
	now := time.Now()
	changed := false
	if req.Operation == "scan" {
		err = conn.QueryRowContext(ctx, "SELECT id,name,COALESCE(jingle,'') FROM users WHERE rfid_uid=?", data.UID).Scan(&userID, &reply.User, &reply.Jingle)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			reply.Status = 404
			reply.Error = "unknown card"
		case err != nil:
			return fail(500, "database read failed")
		case s.Attendance.IsOnCooldown(userID):
			reply.Status = 429
			reply.Error = "too many scans too quickly"
		default:
			var sessionID int64
			err = conn.QueryRowContext(ctx, "SELECT id FROM sessions WHERE user_id=? AND check_out IS NULL", userID).Scan(&sessionID)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = conn.ExecContext(ctx, "INSERT INTO sessions(user_id,check_in) VALUES(?,?)", userID, now)
				reply.Action = "check-in"
			} else if err == nil {
				_, err = conn.ExecContext(ctx, "UPDATE sessions SET check_out=?,check_out_method='rfid' WHERE id=?", now, sessionID)
				reply.Action = "check-out"
			}
			if err != nil {
				log.Error("USB attendance transaction failed", "err", err)
				return fail(500, "database write failed")
			}
			changed = true
		}
	} else {
		result, err := conn.ExecContext(ctx, "UPDATE sessions SET check_out=?,check_out_method='rfid' WHERE check_out IS NULL", now)
		if err != nil {
			return fail(500, "database write failed")
		}
		reply.Count, _ = result.RowsAffected()
		reply.Action = "checkout-all"
		changed = reply.Count > 0
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return fail(500, "response encoding failed")
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO reader_requests(request_id,payload,response,created_at) VALUES(?,?,?,?)", req.ID, payload, string(encoded), now.Unix()); err != nil {
		return fail(500, "receipt write failed")
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fail(500, "attendance commit failed")
	}
	if req.Operation == "scan" {
		if changed {
			s.Attendance.updateLastScanTime(userID)
		}
		s.Attendance.recordScan(data.UID, reply.User, reply.Status != 404, reply.Action)
	}
	if changed {
		TriggerAttendanceChangeCallback()
	}
	log.Info("USB reader request processed", "request_id", req.ID, "operation", req.Operation, "status", reply.Status, "action", reply.Action, "user", reply.User)
	return reply
}

func (r ReaderResponse) String() string { return fmt.Sprintf("%s: %d %s", r.ID, r.Status, r.Action) }
