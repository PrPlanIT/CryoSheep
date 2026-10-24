// Package selfinstall finds, downloads, verifies and installs CryoSheep releases.
package selfinstall

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultSource is the primary forge's releases API for this project.
const DefaultSource = "https://gitlab.prplanit.com/api/v4/projects/PrPlanIT%2FCryoSheep"

// Channel selects which releases are eligible.
type Channel string

const (
	Stable Channel = "stable" // v<semver> without a prerelease suffix
	Dev    Channel = "dev"    // dev-<sha>, one per main build
)

// Release is the subset of a forge release this package needs.
type Release struct {
	Tag    string
	Assets map[string]string // asset name -> download URL
}

type gitlabRelease struct {
	TagName string `json:"tag_name"`
	Assets  struct {
		Links []struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
}

// Eligible reports whether a tag belongs to the channel.
func Eligible(tag string, ch Channel) bool {
	switch ch {
	case Dev:
		return strings.HasPrefix(tag, "dev-")
	case Stable:
		return strings.HasPrefix(tag, "v") && !strings.Contains(tag, "-")
	}
	return false
}

// Latest returns the newest release on the channel. The API lists newest first.
func Latest(ctx context.Context, client *http.Client, source string, ch Channel) (Release, error) {
	var rels []gitlabRelease
	if err := getJSON(ctx, client, source+"/releases?per_page=50", &rels); err != nil {
		return Release{}, err
	}
	for _, r := range rels {
		if !Eligible(r.TagName, ch) {
			continue
		}
		out := Release{Tag: r.TagName, Assets: map[string]string{}}
		for _, l := range r.Assets.Links {
			u := l.DirectAssetURL
			if u == "" {
				u = l.URL
			}
			out.Assets[l.Name] = u
		}
		return out, nil
	}
	return Release{}, fmt.Errorf("no %s release found at %s", ch, source)
}

// ArchiveFor picks the archive asset for an architecture.
func (r Release) ArchiveFor(arch string) (name, url string, err error) {
	suffix := "-linux-" + arch + ".tar.gz"
	for n, u := range r.Assets {
		if strings.HasSuffix(n, suffix) {
			return n, u, nil
		}
	}
	return "", "", fmt.Errorf("release %s has no linux/%s archive", r.Tag, arch)
}

// ParseChecksums reads a SHA256SUMS file into name -> lowercase hex digest.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("malformed SHA256SUMS line: %q", line)
		}
		out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SHA256SUMS is empty")
	}
	return out, nil
}

// Fetch downloads the archive for arch and its SHA256SUMS, refuses any mismatch, and
// returns the cryosheep binary from inside the archive.
func Fetch(ctx context.Context, client *http.Client, r Release, arch string) ([]byte, error) {
	name, url, err := r.ArchiveFor(arch)
	if err != nil {
		return nil, err
	}
	sumsURL, ok := r.Assets["SHA256SUMS"]
	if !ok {
		return nil, fmt.Errorf("release %s publishes no SHA256SUMS; refusing an unverifiable download", r.Tag)
	}
	sumsBody, err := get(ctx, client, sumsURL)
	if err != nil {
		return nil, fmt.Errorf("downloading SHA256SUMS: %w", err)
	}
	sums, err := ParseChecksums(strings.NewReader(string(sumsBody)))
	if err != nil {
		return nil, err
	}
	want, ok := sums[name]
	if !ok {
		return nil, fmt.Errorf("SHA256SUMS does not list %s", name)
	}
	archive, err := get(ctx, client, url)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%s checksum mismatch: got %s, SHA256SUMS lists %s", name, got, want)
	}
	return BinaryFromArchive(archive)
}

// BinaryFromArchive extracts the cryosheep executable from a release tar.gz.
func BinaryFromArchive(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no cryosheep binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && (h.Name == "cryosheep" || strings.HasSuffix(h.Name, "/cryosheep")) {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
}

func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	body, err := get(ctx, client, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// NewClient is the HTTP client used for release lookups and downloads.
func NewClient() *http.Client { return &http.Client{Timeout: 2 * time.Minute} }
