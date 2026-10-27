package qbittorrent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bencodeTorrent builds a minimal single-file torrent so that
// infoHashFromTorrentFile can derive a real v1 infohash from it.
func bencodeTorrent(name string) []byte {
	pieces := strings.Repeat("a", 20)
	info := fmt.Sprintf("d6:lengthi12345e4:name%d:%s12:piece lengthi16384e6:pieces20:%se",
		len(name), name, pieces)
	announce := "http://tracker.example.com/announce"
	return []byte(fmt.Sprintf("d8:announce%d:%s4:info%se", len(announce), announce, info))
}

// expectedHashServer fakes a qBittorrent that answers the add with a bare
// "Ok.". After the add, /torrents/info lists a foreign torrent first and the
// submitted one second, both with the same added_on. When failFirstInfo is set
// the first /torrents/info call (the before-snapshot) returns 500.
func expectedHashServer(wantHash, foreignHash string, failFirstInfo bool, recategorised *string) *httptest.Server {
	var mu sync.Mutex
	added := false
	infoCalls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			mu.Lock()
			added = true
			mu.Unlock()
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			mu.Lock()
			infoCalls++
			first := infoCalls == 1
			isAdded := added
			mu.Unlock()
			if failFirstInfo && first {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if !isAdded {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_, _ = fmt.Fprintf(w, `[{"hash":%q,"name":"foreign","added_on":1000},{"hash":%q,"name":"book.epub","added_on":1000}]`, foreignHash, wantHash)
		case "/api/v2/torrents/setCategory":
			_ = r.ParseForm()
			mu.Lock()
			*recategorised = r.Form.Get("hashes")
			mu.Unlock()
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/setAutoManagement":
			_, _ = w.Write([]byte("Ok."))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func addBookTorrent(t *testing.T, failFirstInfo bool) (got, want, foreign, recategorised string) {
	t.Helper()
	foreign = "ffffffffffffffffffffffffffffffffffffffff"
	payload := bencodeTorrent("book.epub")
	want = infoHashFromTorrentFile(payload)
	if want == "" {
		t.Fatal("test setup: payload does not yield an infohash")
	}
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(indexer.Close)
	srv := expectedHashServer(want, foreign, failFirstInfo, &recategorised)
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL, "admin", "pass")
	allowTorrentFetch(c)
	got, err := c.AddTorrent(context.Background(), indexer.URL+"/torrent", "books", "")
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	return got, want, foreign, recategorised
}

// A torrent added by another writer in the same second (AddedOn has one-second
// resolution) and listed first must not be adopted as this grab.
func TestAddTorrent_RejectsConcurrentForeignTorrent(t *testing.T) {
	got, want, foreign, recategorised := addBookTorrent(t, false)
	if got != want {
		t.Fatalf("AddTorrent returned %q, want the submitted torrent's hash %q", got, want)
	}
	if recategorised == foreign {
		t.Fatal("foreign torrent was recategorised as this grab")
	}
}

// With a failed before-snapshot every torrent is a candidate; the expected hash
// derived from the payload must still pick the right one.
func TestAddTorrent_SnapshotFailureWithDerivableHash(t *testing.T) {
	got, want, _, _ := addBookTorrent(t, true)
	if got != want {
		t.Fatalf("AddTorrent returned %q, want %q", got, want)
	}
}

// A failed before-snapshot and a payload with no infohash must fail closed
// rather than adopt whichever torrent looks newest.
func TestAddTorrent_SnapshotFailureWithoutHashFailsClosed(t *testing.T) {
	var recategorised string
	srv := expectedHashServer("1111111111111111111111111111111111111111", "ffffffffffffffffffffffffffffffffffffffff", true, &recategorised)
	defer srv.Close()

	c := newTestClient(srv.URL, "admin", "pass")
	allowTorrentFetch(c)
	got, err := c.AddTorrent(context.Background(), "not-a-magnet-or-url", "books", "")
	if err == nil {
		t.Fatal("expected an error when the torrent list cannot be read and no infohash is derivable")
	}
	if got != "" {
		t.Fatalf("AddTorrent returned a hash (%q) it could not have identified", got)
	}
	if recategorised != "" {
		t.Fatalf("a torrent was recategorised (%q) despite failing closed", recategorised)
	}
}

// A v2-only torrent (no v1 "pieces") is listed under a different id than the
// SHA-1 of its info dict, so the expected-hash filter must not reject it.
func TestAddTorrent_V2OnlyTorrentStillRecovered(t *testing.T) {
	const listed = "2222222222222222222222222222222222222222"
	payload := []byte("d4:infod9:file treed4:name.d0:d6:lengthi5e11:pieces root32:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaeeee12:meta versioni2e4:name4:book12:piece lengthi16384eee")
	if strings.Contains(string(payload), "6:pieces") {
		t.Fatal("test setup: payload must not carry v1 pieces")
	}
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer indexer.Close()
	var recategorised string
	srv := expectedHashServer(listed, "ffffffffffffffffffffffffffffffffffffffff", false, &recategorised)
	defer srv.Close()

	c := newTestClient(srv.URL, "admin", "pass")
	allowTorrentFetch(c)
	// Both rows share added_on; the first-listed (foreign) one wins the old
	// tie-break, which is acceptable here: no v1 hash exists to verify against.
	got, err := c.AddTorrent(context.Background(), indexer.URL+"/torrent", "books", "")
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if got == "" {
		t.Fatal("expected a recovered hash")
	}
}
