package fincloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

func TestClientInitialLoginReuseAndOneSessionRetry(t *testing.T) {
	var mu sync.Mutex
	logins := 0
	loanCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/admin/access/login":
			logins++
			fmt.Fprintf(writer, `{"status":"ok","data":{"result":{"sessionid":"s-%d"}}}`, logins)
		case "/pinjaman/inquiry/rekening/pinjaman":
			loanCalls++
			if loanCalls == 2 {
				http.Error(writer, "expired", http.StatusUnauthorized)
				return
			}
			writeLoan(writer, request.URL.Query().Get("id"))
		case "/admin/access/logout":
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)
	for _, account := range []string{"one", "two", "three"} {
		if _, err := client.GetLoan(context.Background(), account, testJakarta()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != 2 || loanCalls != 4 {
		t.Fatalf("logins=%d loan calls=%d", logins, loanCalls)
	}
}

func TestClientDistinguishesCredentialRejectionFromNetworkFailure(t *testing.T) {
	t.Run("credentials", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "rejected", http.StatusUnauthorized)
		}))
		defer server.Close()
		client := newTestClient(t, server)
		_, err := client.GetLoan(context.Background(), "one", testJakarta())
		if !errors.Is(err, loan.ErrFincloudCredentials) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("network", func(t *testing.T) {
		client, err := NewClient(Config{
			BaseURL: "https://fincloud.invalid", Username: "system", Password: "secret", LocationID: "000", RoleID: "R-1",
			HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") })},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetLoan(context.Background(), "one", testJakarta())
		if !errors.Is(err, loan.ErrFincloudUnavailable) || errors.Is(err, loan.ErrFincloudCredentials) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("application upstream failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, `{"status":"error","error":{"system":"database unavailable"}}`)
		}))
		defer server.Close()
		client := newTestClient(t, server)
		_, err := client.GetLoan(context.Background(), "one", testJakarta())
		if !errors.Is(err, loan.ErrFincloudUnavailable) || errors.Is(err, loan.ErrFincloudCredentials) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResolveLoanPrimaryAndAlternateFlow(t *testing.T) {
	tests := []struct {
		name       string
		direct     response
		search     response
		refetch    response
		wantID     string
		wantError  error
		wantSearch bool
		wantDetail int
	}{
		{name: "input already primary", direct: okLoan("primary"), wantID: "primary", wantDetail: 1},
		{name: "exactly one primary", direct: notFound(), search: okSearch(`{"id":"primary","noalt":"alternate"}`), refetch: okLoan("primary"), wantID: "primary", wantSearch: true, wantDetail: 2},
		{name: "duplicate same primary", direct: notFound(), search: okSearch(`{"id":"primary","noalt":"alternate"},{"id":"primary","noalt":""}`), refetch: okLoan("primary"), wantID: "primary", wantSearch: true, wantDetail: 2},
		{name: "zero candidate", direct: notFound(), search: okSearch(`{"id":"","noalt":"alternate"},{"id":"wrong","noalt":"other"}`), wantError: loan.ErrNotFound, wantSearch: true, wantDetail: 1},
		{name: "multiple candidates", direct: notFound(), search: okSearch(`{"id":"one"},{"id":"two"}`), wantError: loan.ErrAmbiguousAccountResolution, wantSearch: true, wantDetail: 1},
		{name: "non not-found does not fallback", direct: response{body: `{bad json`}, wantError: loan.ErrFincloudUnavailable, wantDetail: 1},
		{name: "resolved primary refetch mismatch", direct: notFound(), search: okSearch(`{"id":"primary","noalt":"alternate"}`), refetch: okLoan("different"), wantError: loan.ErrInvariant, wantSearch: true, wantDetail: 2},
		{name: "authentication remains distinct", direct: response{body: `{"status":"error","error":{"system":"session expired"}}`}, refetch: response{body: `{"status":"error","error":{"system":"session expired"}}`}, wantError: loan.ErrFincloudSession, wantDetail: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			loginCount, detailCount, searchCount, refetchCount := 0, 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch request.URL.Path {
				case "/admin/access/login":
					loginCount++
					fmt.Fprintf(writer, `{"status":"ok","data":{"result":{"sessionid":"session-%d"}}}`, loginCount)
				case "/admin/access/logout":
					writer.WriteHeader(http.StatusOK)
				case "/pinjaman/inquiry/rekening/pinjaman":
					detailCount++
					selected := test.direct
					if detailCount > 1 {
						refetchCount++
						selected = test.refetch
					}
					selected.write(writer)
				case "/pinjaman/inquiry/rekening/cari":
					searchCount++
					if request.URL.Query().Get("cabang") != "ALL" || request.URL.Query().Get("noalt") != "alternate" || request.URL.Query().Get("pagesize") != "50" || request.Header.Get("sessionid") == "" {
						t.Errorf("bad alternate request: %s header=%q", request.URL.RawQuery, request.Header.Get("sessionid"))
					}
					test.search.write(writer)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server)
			input := "alternate"
			if test.name == "input already primary" {
				input = "primary"
			}
			result, err := client.ResolveLoan(context.Background(), input, testJakarta())
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
			} else if err != nil || result.PrimaryAccount != test.wantID {
				t.Fatalf("loan=%+v error=%v", result, err)
			}
			if (searchCount > 0) != test.wantSearch || detailCount != test.wantDetail {
				t.Fatalf("detail=%d search=%d refetch=%d", detailCount, searchCount, refetchCount)
			}
		})
	}
}

type response struct {
	status int
	body   string
}

func (response response) write(writer http.ResponseWriter) {
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, response.body)
}

func okLoan(id string) response {
	return response{body: fmt.Sprintf(`{"status":"ok","data":{"result":{"id":%q,"plafondlimit":"1200","jangkawaktu":"1 bulan","bungaflat":"12","jadwalangsuran":[{"tanggal":"2026-10-01"}]}}}`, id)}
}

func okSearch(rows string) response {
	return response{body: `{"status":"ok","data":{"result":[` + rows + `]}}`}
}

func notFound() response {
	return response{body: `{"status":"error","description":"Data not found"}`}
}

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(Config{
		BaseURL: server.URL, Username: "system", Password: "secret", LocationID: "000", RoleID: "R-1", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeLoan(writer http.ResponseWriter, id string) {
	okLoan(id).write(writer)
}

func testJakarta() *time.Location { return time.FixedZone("Jakarta", 7*60*60) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestReportDownloadIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/admin/access/login" {
			_, _ = io.WriteString(writer, `{"status":"ok","data":{"result":{"sessionid":"session"}}}`)
			return
		}
		_, _ = io.WriteString(writer, strings.Repeat("x", 11))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Username: "u", Password: "p", LocationID: "l", RoleID: "r", HTTPClient: server.Client(), MaxReportSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DownloadNamedReport(context.Background(), "report"); !errors.Is(err, loan.ErrFincloudUnavailable) {
		t.Fatalf("error = %v", err)
	}
}
