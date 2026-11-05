package navidrome

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEnsureMemberIdentityReconcilesUncertainCreateByCredential(t *testing.T) {
	memberPassword := "durable-member-password"
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/auth/login":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			switch body["username"] {
			case "admin":
				return response(http.StatusOK, `{"id":"admin-1","username":"admin","isAdmin":true,"token":"admin-token"}`, nil), nil
			case "member.one":
				if body["password"] != memberPassword {
					t.Fatal("adapter did not reuse the durable member credential")
				}
				return response(http.StatusOK, `{"id":"member-1","username":"member.one","isAdmin":false,"token":"member-token"}`, nil), nil
			default:
				return response(http.StatusUnauthorized, "", nil), nil
			}
		case "/api/user/":
			if request.Method == http.MethodPost {
				return response(http.StatusConflict, "", nil), nil
			}
			return response(http.StatusOK, `[{"id":"member-1","userName":"member.one","isAdmin":false}]`, nil), nil
		default:
			return response(http.StatusNotFound, "", nil), nil
		}
	})}
	client, err := NewWithHTTPClient("http://navidrome.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.EnsureMemberIdentity(context.Background(), AdminCredential{Username: "admin", Password: "admin-password"}, MemberInput{
		Username: "member.one", Name: "Member One", Email: "member@example.test", Password: memberPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "member-1" {
		t.Fatalf("member id = %q", id)
	}
	resolvedID, exists, err := client.ResolveMemberIdentity(context.Background(), AdminCredential{Username: "admin", Password: "admin-password"}, "member.one", memberPassword)
	if err != nil || !exists || resolvedID != "member-1" {
		t.Fatalf("ResolveMemberIdentity() id=%q exists=%t error=%v", resolvedID, exists, err)
	}
}

func TestAssignAllLibrariesVerifiesReadBack(t *testing.T) {
	var assignedBody string
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/auth/login":
			return response(http.StatusOK, `{"id":"admin-1","username":"admin","isAdmin":true,"token":"admin-token"}`, nil), nil
		case "/api/library/":
			return response(http.StatusOK, `[{"id":2},{"id":1}]`, nil), nil
		case "/api/user/member-1/library/":
			if request.Method == http.MethodPut {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				assignedBody = string(body)
				return response(http.StatusOK, "", nil), nil
			}
			return response(http.StatusOK, `[{"id":1},{"id":2}]`, nil), nil
		default:
			return response(http.StatusNotFound, "", nil), nil
		}
	})}
	client, err := NewWithHTTPClient("http://navidrome.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AssignAllLibraries(context.Background(), AdminCredential{Username: "admin", Password: "admin-password"}, "member-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assignedBody, `"libraryIds":[2,1]`) {
		t.Fatalf("assignment body = %s", assignedBody)
	}
}
