package service

import (
	"errors"
	"strings"

	"office/internal/api/dto"
	"office/internal/domain"
	"office/internal/query"
	"office/internal/repository"
)

type UserService struct {
	Users *repository.UserRepo
}

// CreateUser creates a new user and returns the DTO representation
func (s *UserService) CreateUser(name, rfidUID, discordID string) (*dto.UserResponse, error) {
	if err := s.checkDiscordFree(0, discordID); err != nil {
		return nil, err
	}
	user, err := s.Users.Create(name, rfidUID, discordID)
	if err != nil {
		if isCardTaken(err) {
			return nil, ErrCardInUse
		}
		log.Error("error creating user", "err", err)
		return nil, err
	}

	return toUserDTO(user), nil
}

// GetUserByID retrieves a user by ID and returns the DTO representation
func (s *UserService) GetUserByID(id int64) (*dto.UserResponse, error) {
	user, err := s.Users.FindByID(id)
	if err != nil {
		log.Error("error retrieving user by ID", "id", id, "err", err)
		return nil, err
	}

	return toUserDTO(user), nil
}

// GetUserByDiscordID retrieves a user by Discord ID and returns the domain model
// Used by the Discord bot to find users for checkout operations
func (s *UserService) GetUserByDiscordID(discordID string) (*domain.User, error) {
	user, err := s.Users.FindByDiscordID(discordID)
	if err != nil {
		log.Error("error retrieving user by discord id", "discord_id", discordID, "err", err)
		return nil, err
	}
	return user, nil
}

// UpdateJingle sets a user's chosen sign-in jingle and returns the updated domain model.
// Used by the Discord /jingle command.
func (s *UserService) UpdateJingle(id int64, jingle string) (*domain.User, error) {
	user, err := s.Users.UpdateJingle(id, jingle)
	if err != nil {
		log.Error("error updating user jingle", "id", id, "err", err)
		return nil, err
	}
	return user, nil
}

// ListUsers retrieves users matching the optional filter and returns DTO representations
// Pass empty filter to get all users.
// Set filter.NameLike for search by name.
// Set filter.Limit/Offset for pagination.
func (s *UserService) ListUsers(filter query.UserFilter) ([]*dto.UserResponse, error) {
	users, err := s.Users.List(filter)
	if err != nil {
		log.Error("error listing users", "err", err)
		return nil, err
	}

	dtos := make([]*dto.UserResponse, len(users))
	for i, user := range users {
		dtos[i] = toUserDTO(user)
	}
	return dtos, nil
}

// UpdateUser modifies an existing user's information and returns the DTO representation.
func (s *UserService) UpdateUser(id int64, name, rfidUID, discordID string) (*dto.UserResponse, error) {
	if err := s.checkDiscordFree(id, discordID); err != nil {
		return nil, err
	}
	user, err := s.Users.Update(id, name, rfidUID, discordID)
	if err != nil {
		if isCardTaken(err) {
			return nil, ErrCardInUse
		}
		log.Error("error updating user", "id", id, "err", err)
		return nil, err
	}
	return toUserDTO(user), nil
}

// CountUsers returns total users matching the optional filter.
func (s *UserService) CountUsers(filter query.UserFilter) (int64, error) {
	count, err := s.Users.Count(filter)
	if err != nil {
		log.Error("error counting users", "err", err)
		return 0, err
	}
	return count, nil
}

// DeleteUser removes a single user by ID.
func (s *UserService) DeleteUser(id int64) error {
	err := s.Users.Delete(id)
	if err != nil {
		log.Error("error deleting user", "id", id, "err", err)
		return err
	}
	return nil
}

// DeleteUsers removes users matching the filter (bulk delete).
// Returns the number of users deleted.
func (s *UserService) DeleteUsers(filter query.UserFilter) (int64, error) {
	count, err := s.Users.DeleteWithFilter(filter)
	if err != nil {
		log.Error("error deleting users with filter", "err", err)
		return 0, err
	}
	return count, nil
}

// ListUsersRaw retrieves users matching the optional filter and returns domain models
// Use this for CSV export, Discord bot, or other internal purposes that need full user data including RFID
func (s *UserService) ListUsersRaw(filter query.UserFilter) ([]*domain.User, error) {
	users, err := s.Users.List(filter)
	if err != nil {
		log.Error("error listing users", "err", err)
		return nil, err
	}
	return users, nil
}

// toUserDTO converts a domain.User to dto.UserResponse
func toUserDTO(user *domain.User) *dto.UserResponse {
	return &dto.UserResponse{
		ID:        user.ID,
		Name:      user.Name,
		RFIDUID:   user.RFIDUID,
		DiscordID: user.DiscordID,
	}
}

// ErrDiscordInUse stops one person being registered twice (e.g. once per card).
var ErrDiscordInUse = errors.New("that Discord account is already registered to another member; update that member's card instead")

// ErrCardInUse means the RFID card belongs to another member.
var ErrCardInUse = errors.New("that card is already registered to another member")

func (s *UserService) checkDiscordFree(id int64, discordID string) error {
	if strings.TrimSpace(discordID) == "" {
		return nil
	}
	existing, err := s.Users.FindByDiscordID(discordID)
	if err == nil && existing != nil && existing.ID != id {
		return ErrDiscordInUse
	}
	return nil
}

func isCardTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: users.rfid_uid")
}

// SyncNames sets members' names to their current Discord server names
// (keyed by Discord ID) and returns how many changed.
func (s *UserService) SyncNames(names map[string]string) (int, error) {
	users, err := s.Users.List(query.UserFilter{})
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, u := range users {
		name, ok := names[u.DiscordID]
		if !ok || strings.TrimSpace(name) == "" || name == u.Name {
			continue
		}
		if err := s.Users.UpdateName(u.ID, name); err != nil {
			return changed, err
		}
		log.Info("member name updated from Discord", "id", u.ID, "old", u.Name, "new", name)
		changed++
	}
	return changed, nil
}
