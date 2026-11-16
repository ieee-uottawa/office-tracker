package repository_test

import (
	"path/filepath"
	"testing"
	"time"

	"office/internal/database"
	"office/internal/repository"
	"office/internal/service"

	"github.com/stretchr/testify/require"
)

func TestUserMergeAndDuplicateGuards(t *testing.T) {
	db := database.Open(filepath.Join(t.TempDir(), "users.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	users := &repository.UserRepo{DB: db}
	svc := &service.UserService{Users: users}

	keep, err := users.Create("Vitou", "CARD-NEW", "694")
	require.NoError(t, err)
	drop, err := users.Create("Vitou", "CARD-OLD", "694")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET jingle='mario' WHERE id=?`, drop.ID)
	require.NoError(t, err)
	at := func(h int) time.Time { return time.Date(2026, 11, 14, h, 0, 0, 0, time.UTC) }
	// Past sessions on both rows, and an open (duplicate) visit on each.
	for _, s := range []struct {
		user    int64
		in, out int
	}{{keep.ID, 9, 10}, {drop.ID, 11, 12}, {keep.ID, 14, 0}, {drop.ID, 15, 0}} {
		if s.out == 0 {
			_, err = db.Exec(`INSERT INTO sessions(user_id,check_in) VALUES(?,?)`, s.user, at(s.in))
		} else {
			_, err = db.Exec(`INSERT INTO sessions(user_id,check_in,check_out,check_out_method) VALUES(?,?,?,'rfid')`, s.user, at(s.in), at(s.out))
		}
		require.NoError(t, err)
	}

	require.NoError(t, users.Merge(keep.ID, drop.ID))
	var total, open int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*), SUM(check_out IS NULL) FROM sessions WHERE user_id=?`, keep.ID).Scan(&total, &open))
	require.Equal(t, 3, total, "past sessions move over; the duplicate open visit is dropped")
	require.Equal(t, 1, open)
	_, err = users.FindByID(drop.ID)
	require.Error(t, err)
	_, err = users.FindByRFID("CARD-OLD")
	require.Error(t, err, "the dropped card no longer checks anyone in")
	merged, err := users.FindByID(keep.ID)
	require.NoError(t, err)
	require.Equal(t, "mario", merged.Jingle)

	// The same person can't be registered again with another card.
	_, err = svc.CreateUser("Vitou again", "CARD-OLD", "694")
	require.ErrorIs(t, err, service.ErrDiscordInUse)
	other, err := svc.CreateUser("Jana", "CARD-J", "128")
	require.NoError(t, err)
	_, err = svc.UpdateUser(other.ID, "Jana", "CARD-J", "694")
	require.ErrorIs(t, err, service.ErrDiscordInUse)
	_, err = svc.UpdateUser(other.ID, "Jana", "CARD-NEW", "128")
	require.ErrorIs(t, err, service.ErrCardInUse)
	_, err = svc.UpdateUser(other.ID, "Jana - VP", "CARD-J", "128")
	require.NoError(t, err, "a member keeps their own Discord ID when edited")

	changed, err := svc.SyncNames(map[string]string{"694": "Vitou - 1st Year Coordinator", "128": "Jana - VP", "999": "Nobody"})
	require.NoError(t, err)
	require.Equal(t, 1, changed)
	merged, _ = users.FindByID(keep.ID)
	require.Equal(t, "Vitou - 1st Year Coordinator", merged.Name)
}
