package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRouter_PrefixMatchRespectsBoundary regression-tests a bug where
// `extractRoutePrefix` and `Router.ServeHTTP` matched prefixes by raw
// string-prefix, so a path like `/kumo-audit-bad-bucket` was routed to
// the `/kumo` prefix router (which only handles `/_kumo/*` and
// similar), shadowing the S3 wildcard route `PUT /{bucket}` and
// returning 404. The fix requires the prefix to be followed by `/` or
// end-of-string.
func TestRouter_PrefixMatchRespectsBoundary(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter(logger)

	called := ""

	// `/kumo` is a registered prefix (used by /_kumo/health etc.).
	r.Handle("GET", "/kumo/health", func(w http.ResponseWriter, _ *http.Request) {
		called = "kumo-health"

		w.WriteHeader(http.StatusOK)
	})

	// `/{bucket}` is the S3-style wildcard route. With the bug, a
	// PUT to `/kumo-audit-bad-bucket` would be sent to the /kumo
	// prefix router (no matching pattern) → 404.
	r.Handle("PUT", "/{bucket}", func(w http.ResponseWriter, _ *http.Request) {
		called = "bucket-put"

		w.WriteHeader(http.StatusOK)
	})

	// A bucket whose name *equals* an admin prefix (e.g. PUT /kumo) is
	// an unavoidable shadow until kumo grows a Host- or scheme-based
	// admin-route discriminator. The cases below cover only the
	// previously-broken substring case that the fix resolves.
	cases := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"prefix exact", "GET", "/kumo/health", "kumo-health"},
		{"bucket name shares prefix substring", "PUT", "/kumo-audit-bad-bucket", "bucket-put"},
		{"bucket name with longer admin prefix substring", "PUT", "/lambda-deploy-bucket", "bucket-put"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = ""

			req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if called != tc.want {
				t.Fatalf("path=%s: got handler %q, want %q (status=%d)",
					tc.path, called, tc.want, rec.Code)
			}
		})
	}
}

// TestRouter_JWKSRouteNotShadowedByS3 guards the Cognito JWKS endpoint
// `GET /{userPoolId}/.well-known/jwks.json`. Its pattern formally collides
// with S3's wildcard routes in the shared mux: GET implies HEAD, so the JWKS
// GET overlaps S3's explicit `HEAD /{bucket}/{key...}` with neither pattern
// more specific, which made http.ServeMux panic at registration. The router
// puts JWKS on a dedicated mux. This test registers the exact conflicting
// trio and asserts: no registration panic, JWKS wins on its path (with the
// userPoolId path value intact), and arbitrary `{bucket}/{key}` still reaches
// S3 for both GET and HEAD.
func TestRouter_JWKSRouteNotShadowedByS3(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRouter(logger)

	called := ""
	gotUserPoolID := ""

	// Registering these three in one mux is what used to panic.
	r.Handle("GET", "/{userPoolId}/.well-known/jwks.json", func(w http.ResponseWriter, req *http.Request) {
		called = "jwks"
		gotUserPoolID = req.PathValue("userPoolId")

		w.WriteHeader(http.StatusOK)
	})

	r.Handle("GET", "/{bucket}/{key...}", func(w http.ResponseWriter, _ *http.Request) {
		called = "s3-get"

		w.WriteHeader(http.StatusOK)
	})

	r.Handle("HEAD", "/{bucket}/{key...}", func(w http.ResponseWriter, _ *http.Request) {
		called = "s3-head"

		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name           string
		method         string
		path           string
		want           string
		wantUserPoolID string
	}{
		{"jwks GET", "GET", "/us-east-1_abc123def/.well-known/jwks.json", "jwks", "us-east-1_abc123def"},
		{"s3 object GET", "GET", "/mybucket/some/key.txt", "s3-get", ""},
		{"s3 object HEAD", "HEAD", "/mybucket/some/key.txt", "s3-head", ""},
		{"s3 deep key GET", "GET", "/mybucket/a/b/c/d.json", "s3-get", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = ""
			gotUserPoolID = ""

			req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if called != tc.want {
				t.Fatalf("%s %s: got handler %q, want %q (status=%d)", tc.method, tc.path, called, tc.want, rec.Code)
			}

			if gotUserPoolID != tc.wantUserPoolID {
				t.Fatalf("%s %s: got userPoolId %q, want %q", tc.method, tc.path, gotUserPoolID, tc.wantUserPoolID)
			}
		})
	}
}

// TestExtractRoutePrefix_BoundaryGuard makes sure pattern registration
// itself doesn't classify a wildcard like `/{bucket}` as a `/kumo`
// prefixed route.
func TestExtractRoutePrefix_BoundaryGuard(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		want    string
	}{
		{"/kumo/health", "/kumo"},
		{"/lambda/2015-03-31/functions", "/lambda"},
		{"/{bucket}", ""},
		{"/{bucket}/{key...}", ""},
		{"/kumosomething", ""}, // no slash boundary → not a prefix
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			got := extractRoutePrefix(tc.pattern)
			if got != tc.want {
				t.Errorf("extractRoutePrefix(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
		})
	}
}
