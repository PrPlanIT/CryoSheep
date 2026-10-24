package selfinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEligible(t *testing.T) {
	cases := []struct {
		tag  string
		ch   Channel
		want bool
	}{
		{"v0.1.0", Stable, true},
		{"v0.1.0-rc.1", Stable, false},
		{"dev-b14fceb", Stable, false},
		{"dev-b14fceb", Dev, true},
		{"v0.1.0", Dev, false},
		{"latest-dev", Dev, false},
	}
	for _, c := range cases {
		if got := Eligible(c.tag, c.ch); got != c.want {
			t.Errorf("Eligible(%q, %s) = %v, want %v", c.tag, c.ch, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("a", 64)
	got, err := ParseChecksums(strings.NewReader(h + "  CryoSheep-1-linux-amd64.tar.gz\n\n" + strings.ToUpper(h) + " *b.tar.gz\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got["CryoSheep-1-linux-amd64.tar.gz"] != h || got["b.tar.gz"] != h {
		t.Errorf("parsed %v", got)
	}
	if _, err := ParseChecksums(strings.NewReader("nope\n")); err == nil {
		t.Error("malformed line must fail")
	}
	if _, err := ParseChecksums(strings.NewReader("\n")); err == nil {
		t.Error("empty file must fail")
	}
}

func archiveWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func forge(t *testing.T, archive []byte, sums string, withSums bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		links := fmt.Sprintf(`{"name":"CryoSheep-latest-dev-linux-amd64.tar.gz","direct_asset_url":"%s/a"}`, srv.URL)
		if withSums {
			links += fmt.Sprintf(`,{"name":"SHA256SUMS","url":"%s/s"}`, srv.URL)
		}
		fmt.Fprintf(w, `[{"tag_name":"v9.9.9","assets":{"links":[]}},{"tag_name":"dev-abc1234","assets":{"links":[%s]}}]`, links)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/s", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchVerifiesChecksum(t *testing.T) {
	archive := archiveWith(t, "cryosheep", []byte("binary"))
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:]) + "  CryoSheep-latest-dev-linux-amd64.tar.gz\n"
	ctx := context.Background()

	srv := forge(t, archive, good, true)
	rel, err := Latest(ctx, srv.Client(), srv.URL, Dev)
	if err != nil || rel.Tag != "dev-abc1234" {
		t.Fatalf("Latest: %v %+v", err, rel)
	}
	bin, err := Fetch(ctx, srv.Client(), rel, "amd64")
	if err != nil || string(bin) != "binary" {
		t.Fatalf("Fetch: %v %q", err, bin)
	}

	bad := forge(t, archive, strings.Repeat("0", 64)+"  CryoSheep-latest-dev-linux-amd64.tar.gz\n", true)
	rel, _ = Latest(ctx, bad.Client(), bad.URL, Dev)
	if _, err := Fetch(ctx, bad.Client(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("checksum mismatch must fail, got %v", err)
	}

	none := forge(t, archive, "", false)
	rel, _ = Latest(ctx, none.Client(), none.URL, Dev)
	if _, err := Fetch(ctx, none.Client(), rel, "amd64"); err == nil || !strings.Contains(err.Error(), "no SHA256SUMS") {
		t.Errorf("missing SHA256SUMS must fail, got %v", err)
	}
	if _, err := Fetch(ctx, srv.Client(), rel, "arm64"); err == nil {
		t.Error("missing architecture must fail")
	}
}

func TestInstallConverges(t *testing.T) {
	dir := t.TempDir()
	BinaryPath = filepath.Join(dir, "bin", "cryosheep")
	UnitDir = filepath.Join(dir, "units")
	ChannelFile = filepath.Join(dir, "etc", "channel")
	var calls []string
	Systemctl = func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	units := fstest.MapFS{"cryosheep-stasis.service": {Data: []byte("a")}, "cryosheep-wake.service": {Data: []byte("b")}}

	rep, err := Install([]byte("bin1"), units, Dev, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) == 0 || InstalledChannel() != Dev {
		t.Fatalf("first install: %v channel=%s", rep, InstalledChannel())
	}
	if fi, _ := os.Stat(BinaryPath); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", fi.Mode())
	}
	calls = nil
	rep, err = Install([]byte("bin1"), units, Dev, false)
	if err != nil || len(rep) != 0 || len(calls) != 0 {
		t.Errorf("second install must change nothing: %v %v %v", rep, calls, err)
	}
	if _, err := Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(BinaryPath); !os.IsNotExist(err) {
		t.Error("remove left the binary")
	}
}
