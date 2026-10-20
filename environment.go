package autotix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// Environment reporting feeds Autotix Vulnerability Scanning: the Go version
// and the modules compiled into the binary are sent to /api/telemetry/env,
// stored per site URL, and checked daily against the OSV.dev advisory
// database. It mirrors the JavaScript SDK's reportEnvironment and the PHP
// SDK's EnvReporter.

const (
	envReportPath   = "/api/telemetry/env"
	webhookPath     = "/api/webhook/error"
	envPlatform     = "go"
	envEcosystem    = "Go"
	maxEnvResponse  = 64 << 10
	maxEnvErrorText = 200
)

// ErrNoSiteURL is returned by ReportEnvironment when the client was created
// without WithSiteURL. Autotix keys each service's module inventory on its
// site URL, so there is nothing to report against without one.
var ErrNoSiteURL = errors.New("autotix: environment reporting needs WithSiteURL")

// ErrEnvironmentNeedsToken is returned by ReportEnvironment when the client
// only has an HMAC secret. The environment endpoint authenticates with the
// webhook token.
var ErrEnvironmentNeedsToken = errors.New("autotix: environment reporting needs the webhook token, an HMAC secret alone is not accepted")

// EnvironmentResult is the server's answer to an environment report.
type EnvironmentResult struct {
	// Status is the HTTP status code of the response.
	Status int
	// Stored is false when the report was accepted but not kept because
	// Vulnerability Scanning is not enabled for the org or project. Reason
	// says why.
	Stored bool
	// Changed reports whether the inventory differs from the previous report
	// for the same site URL. It is only meaningful when Stored is true.
	Changed bool
	// Reason explains why a report was not stored.
	Reason string
	// NextCheckDays is the server's suggested wait before reporting again.
	NextCheckDays int
	// Packages is how many packages were sent, the standard library included.
	Packages int
}

// ReportEnvironment sends the Go version and the modules compiled into this
// binary to Autotix for Vulnerability Scanning, and waits for the answer.
// New already does this once in the background when WithSiteURL is set, so
// call it yourself only from tests, CLIs, or after turning that off with
// WithEnvironmentReportOnStart(false).
//
// The request is bounded by ctx and by the client timeout. A non-2xx answer
// is returned as an error alongside the result, whose Status is set. A nil
// client reports nothing and returns a zero result.
func (c *Client) ReportEnvironment(ctx context.Context) (EnvironmentResult, error) {
	if c == nil {
		return EnvironmentResult{}, nil
	}
	if err := c.environmentReportable(); err != nil {
		return EnvironmentResult{}, err
	}

	info, _ := c.cfg.readBuildInfo()
	report := buildEnvReport(c.cfg.siteURL, c.cfg.project, info, runtime.Version())
	res := EnvironmentResult{Packages: len(report.Packages)}

	body, err := json.Marshal(report)
	if err != nil {
		return res, fmt.Errorf("autotix: encode environment report: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.envURL, bytes.NewReader(body))
	if err != nil {
		return res, fmt.Errorf("autotix: environment report: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "autotix-go/"+Version)
	req.Header.Set("X-Webhook-Token", c.cfg.token)

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("autotix: environment report: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxEnvResponse))

	var answer struct {
		Stored        bool   `json:"stored"`
		Changed       bool   `json:"changed"`
		Reason        string `json:"reason"`
		NextCheckDays int    `json:"nextCheckDays"`
		Error         string `json:"error"`
	}
	_ = json.Unmarshal(raw, &answer) // a non-JSON body leaves the zero values
	res.Status = resp.StatusCode
	res.Stored = answer.Stored
	res.Changed = answer.Changed
	res.Reason = answer.Reason
	res.NextCheckDays = answer.NextCheckDays

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if answer.Error != "" {
			return res, fmt.Errorf("autotix: environment report rejected with status %d: %s",
				resp.StatusCode, truncate(answer.Error, maxEnvErrorText))
		}
		return res, fmt.Errorf("autotix: environment report rejected with status %d", resp.StatusCode)
	}
	return res, nil
}

// environmentReportable says why a report cannot be made, or returns nil.
func (c *Client) environmentReportable() error {
	if c.cfg.siteURL == "" {
		return ErrNoSiteURL
	}
	if c.cfg.token == "" {
		return ErrEnvironmentNeedsToken
	}
	return nil
}

// startEnvironmentReport sends one report in the background after New. It
// never panics and never blocks the caller; Close waits for it within its
// context and cancels it when that context ends.
func (c *Client) startEnvironmentReport() {
	if !c.cfg.envReportOnStart {
		return
	}
	if err := c.environmentReportable(); err != nil {
		c.log.Debug("Autotix: environment report skipped", "reason", err.Error())
		return
	}
	c.background.Add(1)
	go func() {
		defer c.background.Done()
		defer func() {
			if r := recover(); r != nil {
				c.log.Warn("Autotix: environment report panicked", "panic", fmt.Sprint(r))
			}
		}()

		res, err := c.ReportEnvironment(c.bgCtx)
		switch {
		case err != nil && c.bgCtx.Err() != nil:
			c.log.Debug("Autotix: environment report canceled by Close")
		case res.Status == http.StatusTooManyRequests:
			// Expected when many replicas of one service start together:
			// they all report the same inventory, so one getting through
			// is enough.
			c.log.Info("Autotix: environment report rate limited, skipped until the next start")
		case err != nil:
			c.log.Warn("Autotix: environment report failed", "error", err.Error())
		case !res.Stored:
			c.log.Info("Autotix: environment report not stored", "reason", res.Reason)
		default:
			c.log.Debug("Autotix: environment reported", "packages", res.Packages, "changed", res.Changed)
		}
	}()
}

// --- report building ---------------------------------------------------------

type envPackage struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Ecosystem string `json:"ecosystem"`
}

type envReport struct {
	SiteURL         string       `json:"siteUrl"`
	Platform        string       `json:"platform"`
	PlatformVersion string       `json:"platformVersion,omitempty"`
	Runtime         string       `json:"runtime,omitempty"`
	Packages        []envPackage `json:"packages"`
	ProjectKey      string       `json:"projectKey,omitempty"`
}

// buildEnvReport assembles the report body. Versions are sent in the bare
// semver form OSV's Go advisories use ("0.17.0", "1.22.3"). OSV tolerates a
// leading "v", but treats the toolchain form "go1.22.3" as unparseable and
// over-matches it, so the SDK normalizes before sending. The server
// normalizes again when it queries OSV, as a guard for other reporters.
func buildEnvReport(siteURL, project string, info *debug.BuildInfo, goVersion string) envReport {
	packages := modulePackages(info)

	platformVersion := osvGoVersion(goVersion)
	if platformVersion != "" {
		packages = append(packages, envPackage{Name: "stdlib", Version: platformVersion, Ecosystem: envEcosystem})
	} else {
		// A development toolchain has no release to look up; report it as is
		// and leave the standard library out.
		platformVersion = strings.TrimSpace(goVersion)
	}

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}
		return packages[i].Version < packages[j].Version
	})

	report := envReport{
		SiteURL:         siteURL,
		Platform:        envPlatform,
		PlatformVersion: platformVersion,
		Packages:        packages,
		ProjectKey:      project,
	}
	if platformVersion != "" {
		report.Runtime = "go " + platformVersion
	}
	return report
}

// modulePackages lists the dependency modules compiled into the binary. A
// replaced module is reported as its replacement, since that is the code
// that runs; a replacement by local directory has no version to look up and
// is skipped, as is the main module.
func modulePackages(info *debug.BuildInfo) []envPackage {
	if info == nil {
		return []envPackage{}
	}
	out := make([]envPackage, 0, len(info.Deps)+1)
	for _, dep := range info.Deps {
		if dep == nil {
			continue
		}
		mod := dep
		if dep.Replace != nil {
			mod = dep.Replace
		}
		if mod.Path == "" || mod.Path == info.Main.Path {
			continue
		}
		if mod.Version == "" || mod.Version == "(devel)" {
			continue
		}
		out = append(out, envPackage{
			Name:      mod.Path,
			Version:   strings.TrimPrefix(mod.Version, "v"),
			Ecosystem: envEcosystem,
		})
	}
	return out
}

// goReleaseRE matches a toolchain release as runtime.Version reports it:
// go1.22.3, go1.20 (x.y.0 releases before Go 1.21), go1.22rc1.
var goReleaseRE = regexp.MustCompile(`^go(\d+)\.(\d+)(?:\.(\d+))?(?:(rc|beta)(\d+))?$`)

// osvGoVersion converts a runtime.Version string to the semver OSV uses for
// the "stdlib" package, or "" for a development toolchain. A GOEXPERIMENT
// suffix ("go1.22.0 X:boringcrypto") is ignored.
func osvGoVersion(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	m := goReleaseRE.FindStringSubmatch(fields[0])
	if m == nil {
		return ""
	}
	patch := m[3]
	if patch == "" {
		patch = "0"
	}
	v := m[1] + "." + m[2] + "." + patch
	if m[4] != "" {
		v += "-" + m[4] + "." + m[5]
	}
	return v
}

// envReportURL derives the environment endpoint from the error endpoint, so
// a self-hosted or staging endpoint set with WithEndpoint is honored. A path
// ending in /api/webhook/error keeps its prefix; any other path is replaced.
func envReportURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	prefix := ""
	if p := strings.TrimSuffix(u.Path, "/"); strings.HasSuffix(p, webhookPath) {
		prefix = strings.TrimSuffix(p, webhookPath)
	}
	out := *u
	out.Path = prefix + envReportPath
	out.RawPath = ""
	out.RawQuery = ""
	out.ForceQuery = false
	out.Fragment = ""
	out.RawFragment = ""
	return out.String()
}

func validateSiteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("autotix: invalid site URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("autotix: site URL must be an http(s) URL, got %q", raw)
	}
	return nil
}
