package repository

import (
	"database/sql"
	"strings"

	"office/internal/domain"
	"office/internal/query"
)

type UserRepo struct {
	DB *sql.DB
}

// FindByRFID retrieves a user from the database based on their RFID UID. If no user is found, it returns nil and an error.
func (r *UserRepo) FindByRFID(uid string) (*domain.User, error) {
	row := r.DB.QueryRow(`
		SELECT id, name, rfid_uid, discord_id, COALESCE(jingle, ''), created_at
		FROM users WHERE rfid_uid = ?`, uid)

	var u domain.User

	err := row.Scan(&u.ID, &u.Name, &u.RFIDUID, &u.DiscordID, &u.Jingle, &u.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &u, nil
}

// FindByID retrieves a user from the database based on their ID. If no user is found, it returns nil and an error.
func (r *UserRepo) FindByID(id int64) (*domain.User, error) {
	row := r.DB.QueryRow(`
		SELECT id, name, rfid_uid, discord_id, COALESCE(jingle, ''), created_at
		FROM users WHERE id = ?`, id)

	var u domain.User

	err := row.Scan(&u.ID, &u.Name, &u.RFIDUID, &u.DiscordID, &u.Jingle, &u.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &u, nil
}

// FindByDiscordID retrieves a user from the database based on their Discord ID. If no user is found, it returns nil and an error.
func (r *UserRepo) FindByDiscordID(discordID string) (*domain.User, error) {
	row := r.DB.QueryRow(`
		SELECT id, name, rfid_uid, discord_id, COALESCE(jingle, ''), created_at
		FROM users WHERE discord_id = ?`, discordID)

	var u domain.User

	err := row.Scan(&u.ID, &u.Name, &u.RFIDUID, &u.DiscordID, &u.Jingle, &u.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &u, nil
}

// UpdateJingle sets a user's chosen sign-in jingle by ID.
func (r *UserRepo) UpdateJingle(id int64, jingle string) (*domain.User, error) {
	_, err := r.DB.Exec(`UPDATE users SET jingle = ? WHERE id = ?`, jingle, id)
	if err != nil {
		return nil, err
	}
	return r.FindByID(id)
}

// List retrieves users matching the filter.
func (r *UserRepo) List(filter query.UserFilter) ([]*domain.User, error) {
	q := `SELECT id, name, rfid_uid, discord_id, created_at
          FROM users`

	var where []string
	var args []interface{}

	if filter.NameLike != nil {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+*filter.NameLike+"%")
	}

	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	// Handle ordering - default to ASC
	orderDir := "ASC"
	if filter.OrderBy == "desc" {
		orderDir = "DESC"
	}

	// Handle sort field - default to name
	sortField := "name"
	if filter.SortBy == "created_at" {
		sortField = "created_at"
	}
	q += " ORDER BY " + sortField + " " + orderDir

	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
	}

	rows, err := r.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Name, &u.RFIDUID, &u.DiscordID, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, &u)
	}
	return users, nil
}

// Count returns total users matching the filter (ignores limit/offset).
func (r *UserRepo) Count(filter query.UserFilter) (int64, error) {
	q := `SELECT COUNT(*) FROM users`

	var where []string
	var args []interface{}

	if filter.NameLike != nil {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+*filter.NameLike+"%")
	}

	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	var count int64
	if err := r.DB.QueryRow(q, args...).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}

// Create adds a new user to the database with the provided name, RFID UID, and Discord ID. It returns the created User object and an error if the operation fails.
func (r *UserRepo) Create(name, rfidUID, discordID string) (*domain.User, error) {
	result, err := r.DB.Exec(`
		INSERT INTO users(name, rfid_uid, discord_id)
		VALUES (?, ?, ?)`, name, rfidUID, discordID)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &domain.User{
		ID:        id,
		Name:      name,
		RFIDUID:   rfidUID,
		DiscordID: discordID,
	}, nil
}

// Update modifies an existing user's information.
func (r *UserRepo) Update(id int64, name, rfidUID, discordID string) (*domain.User, error) {
	_, err := r.DB.Exec(`
		UPDATE users 
		SET name = ?, rfid_uid = ?, discord_id = ?
		WHERE id = ?`, name, rfidUID, discordID, id)
	if err != nil {
		return nil, err
	}

	// Return updated user
	return r.FindByID(id)
}

// Delete removes a single user by ID.
func (r *UserRepo) Delete(id int64) error {
	_, err := r.DB.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// DeleteWithFilter removes users matching the filter (bulk delete).
// Returns the number of users deleted.
func (r *UserRepo) DeleteWithFilter(filter query.UserFilter) (int64, error) {
	q := `DELETE FROM users`
	var where []string
	var args []interface{}

	if filter.NameLike != nil {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+*filter.NameLike+"%")
	}

	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	result, err := r.DB.Exec(q, args...)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Merge moves everything recorded for user `drop` onto user `keep` and then
// deletes `drop` (and its card). When both have an open session, the one on
// `drop` is a duplicate of the same visit and is removed.
func (r *UserRepo) Merge(keep, drop int64) error {
	tx, err := r.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var keepOpen, dropOpen int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=? AND check_out IS NULL`, keep).Scan(&keepOpen); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=? AND check_out IS NULL`, drop).Scan(&dropOpen); err != nil {
		return err
	}
	if keepOpen > 0 && dropOpen > 0 {
		if _, err = tx.Exec(`DELETE FROM sessions WHERE user_id=? AND check_out IS NULL`, drop); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE sessions SET user_id=? WHERE user_id=?`, keep, drop); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE users SET jingle=(SELECT jingle FROM users WHERE id=?) WHERE id=? AND COALESCE(jingle,'')=''`, drop, keep); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM users WHERE id=?`, drop); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateName changes a member's display name.
func (r *UserRepo) UpdateName(id int64, name string) error {
	_, err := r.DB.Exec(`UPDATE users SET name=? WHERE id=?`, name, id)
	return err
}
