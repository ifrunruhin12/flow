package navidrome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type AdminCredential struct {
	Username string
	Password string
}

type MemberInput struct {
	Username string
	Name     string
	Email    string
	Password string
}

type Library struct {
	ID int `json:"id"`
}

func (c *Client) EnsureMemberIdentity(ctx context.Context, admin AdminCredential, input MemberInput) (string, error) {
	_, session, err := c.Login(ctx, admin.Username, admin.Password)
	if err != nil {
		return "", fmt.Errorf("authenticate Navidrome administrator: %w", err)
	}
	member := map[string]any{
		"userName": input.Username,
		"name":     input.Name,
		"isAdmin":  false,
		"password": input.Password,
	}
	if input.Email != "" {
		member["email"] = input.Email
	}
	payload, err := json.Marshal(member)
	if err != nil {
		return "", err
	}
	response, err := session.do(ctx, http.MethodPost, "api/user/", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
	response.Body.Close()
	createStatus := response.StatusCode

	users, err := session.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	var memberID string
	for _, user := range users {
		if strings.EqualFold(user.Username, input.Username) {
			memberID = user.ID
			break
		}
	}
	if memberID == "" {
		if createStatus != http.StatusOK && createStatus != http.StatusCreated {
			return "", &HTTPError{Operation: "create member", StatusCode: createStatus}
		}
		return "", errors.New("Navidrome created member was not returned by user read-back")
	}
	memberLogin, _, err := c.Login(ctx, input.Username, input.Password)
	if err != nil || memberLogin.ID != memberID || memberLogin.IsAdmin {
		if createStatus != http.StatusOK && createStatus != http.StatusCreated {
			return "", errors.New("Navidrome username collision could not be authenticated with the durable credential")
		}
		return "", errors.New("Navidrome member credential verification failed")
	}
	return memberID, nil
}

func (c *Client) ResolveMemberIdentity(ctx context.Context, admin AdminCredential, username, password string) (string, bool, error) {
	_, session, err := c.Login(ctx, admin.Username, admin.Password)
	if err != nil {
		return "", false, fmt.Errorf("authenticate Navidrome administrator: %w", err)
	}
	users, err := session.ListUsers(ctx)
	if err != nil {
		return "", false, err
	}
	memberID := ""
	for _, user := range users {
		if strings.EqualFold(user.Username, username) {
			memberID = user.ID
			break
		}
	}
	if memberID == "" {
		return "", false, nil
	}
	memberLogin, _, err := c.Login(ctx, username, password)
	if err != nil || memberLogin.ID != memberID || memberLogin.IsAdmin {
		return "", false, errors.New("Navidrome username collision could not be authenticated with the durable credential")
	}
	return memberID, true, nil
}

func (c *Client) AssignAllLibraries(ctx context.Context, admin AdminCredential, userID string) error {
	_, session, err := c.Login(ctx, admin.Username, admin.Password)
	if err != nil {
		return fmt.Errorf("authenticate Navidrome administrator: %w", err)
	}
	libraries, err := session.ListLibraries(ctx)
	if err != nil {
		return err
	}
	if len(libraries) == 0 {
		return errors.New("Navidrome has no library available for member assignment")
	}
	ids := make([]int, len(libraries))
	for index, library := range libraries {
		ids[index] = library.ID
	}
	if err := session.AssignLibraries(ctx, userID, ids); err != nil {
		return err
	}
	assigned, err := session.UserLibraries(ctx, userID)
	if err != nil {
		return err
	}
	if !sameIntegers(ids, assigned) {
		return errors.New("Navidrome member library assignment read-back did not match")
	}
	return nil
}

func (c *Client) DeleteMember(ctx context.Context, admin AdminCredential, userID string) error {
	_, session, err := c.Login(ctx, admin.Username, admin.Password)
	if err != nil {
		return fmt.Errorf("authenticate Navidrome administrator: %w", err)
	}
	response, err := session.do(ctx, http.MethodDelete, "api/user/"+url.PathEscape(userID)+"/", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return &HTTPError{Operation: "delete member", StatusCode: response.StatusCode}
	}
	return nil
}

func (s *Session) ListLibraries(ctx context.Context) ([]Library, error) {
	response, err := s.do(ctx, http.MethodGet, "api/library/", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
		return nil, &HTTPError{Operation: "list libraries", StatusCode: response.StatusCode}
	}
	var libraries []Library
	if err := decodeJSON(response.Body, &libraries); err != nil {
		return nil, fmt.Errorf("decode Navidrome libraries: %w", err)
	}
	return libraries, nil
}

func (s *Session) AssignLibraries(ctx context.Context, userID string, libraryIDs []int) error {
	payload, err := json.Marshal(map[string]any{"libraryIds": libraryIDs})
	if err != nil {
		return err
	}
	response, err := s.do(ctx, http.MethodPut, "api/user/"+url.PathEscape(userID)+"/library/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
	if response.StatusCode != http.StatusOK {
		return &HTTPError{Operation: "assign member libraries", StatusCode: response.StatusCode}
	}
	return nil
}

func (s *Session) UserLibraries(ctx context.Context, userID string) ([]int, error) {
	response, err := s.do(ctx, http.MethodGet, "api/user/"+url.PathEscape(userID)+"/library/", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8<<10)
		return nil, &HTTPError{Operation: "read member libraries", StatusCode: response.StatusCode}
	}
	var libraries []Library
	if err := decodeJSON(response.Body, &libraries); err != nil {
		return nil, fmt.Errorf("decode member libraries: %w", err)
	}
	ids := make([]int, len(libraries))
	for index, library := range libraries {
		ids[index] = library.ID
	}
	return ids, nil
}

func sameIntegers(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]int(nil), left...)
	right = append([]int(nil), right...)
	sort.Ints(left)
	sort.Ints(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
