package lambda

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessExecutor_Supports(t *testing.T) {
	t.Parallel()

	e := newProcessExecutor(newRuntimeBroker(), "127.0.0.1:4566")

	cases := []struct {
		name string
		fn   *Function
		want bool
	}{
		{"provided.al2023 with code", &Function{Runtime: "provided.al2023", Code: &FunctionCode{ZipFile: []byte("x")}}, true},
		{"go1.x with code", &Function{Runtime: "go1.x", Code: &FunctionCode{ZipFile: []byte("x")}}, true},
		{"managed runtime unsupported", &Function{Runtime: "python3.12", Code: &FunctionCode{ZipFile: []byte("x")}}, false},
		{"no code", &Function{Runtime: "provided.al2023"}, false},
		{"nil function", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.supports(tc.fn); got != tc.want {
				t.Errorf("supports = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProcessExecutor_ExtractBootstrap(t *testing.T) {
	t.Parallel()

	e := newProcessExecutor(newRuntimeBroker(), "127.0.0.1:4566")
	e.workDir = t.TempDir()

	fn := &Function{FunctionName: "fn", Code: &FunctionCode{ZipFile: zipBytes(t, bootstrapEntry, []byte("#!/bin/true\n"))}}

	path, err := e.extractBootstrap(fn)
	if err != nil {
		t.Fatalf("extractBootstrap: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat extracted bootstrap: %v", err)
	}

	if info.Mode()&0o111 == 0 {
		t.Errorf("bootstrap not executable: mode %v", info.Mode())
	}

	got, err := os.ReadFile(path) //nolint:gosec // test-controlled extracted path
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "#!/bin/true\n" {
		t.Errorf("bootstrap content: got %q", got)
	}
}

func TestProcessExecutor_BuildEnv(t *testing.T) {
	t.Parallel()

	e := newProcessExecutor(newRuntimeBroker(), "127.0.0.1:4566")
	fn := &Function{
		FunctionName: "fn",
		Handler:      "bootstrap",
		Environment:  &Environment{Variables: map[string]string{"TABLE_NAME": "t", "AWS_REGION": "ap-northeast-1"}},
	}

	env := envMap(e.buildEnv(fn))

	checks := map[string]string{
		"AWS_LAMBDA_RUNTIME_API":   "127.0.0.1:4566/_runtime/fn",
		"AWS_LAMBDA_FUNCTION_NAME": "fn",
		"TABLE_NAME":               "t",
		"AWS_REGION":               "ap-northeast-1", // function var overrides the injected default
	}

	for k, want := range checks {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}

	if env["AWS_ACCESS_KEY_ID"] == "" {
		t.Errorf("dummy AWS credentials not injected")
	}
}

// TestProcessExecutor_InvokeRunsBootstrap exercises the full executor path: a
// real bootstrap (a stdlib Runtime API client) is built, zipped, registered as a
// function, and invoked. kumo launches it via the process executor, the bootstrap
// polls kumo's Runtime API, and InvokeSync returns its response.
func TestProcessExecutor_InvokeRunsBootstrap(t *testing.T) {
	t.Parallel()

	zipped := zipBootstrap(t, buildTestBootstrap(t))

	store := NewMemoryStorage(defaultBaseURL)
	svc := New(store, defaultBaseURL)

	// Serve only the Runtime API routes the bootstrap polls.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_runtime/{functionName}/2018-06-01/runtime/invocation/next", svc.RuntimeNext)
	mux.HandleFunc("POST /_runtime/{functionName}/2018-06-01/runtime/invocation/{requestId}/response", svc.RuntimeResponse)
	mux.HandleFunc("POST /_runtime/{functionName}/2018-06-01/runtime/invocation/{requestId}/error", svc.RuntimeError)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	svc.executor = newProcessExecutor(svc.broker, strings.TrimPrefix(ts.URL, "http://"))
	t.Cleanup(svc.executor.close)

	if _, err := store.CreateFunction(t.Context(), &CreateFunctionRequest{
		FunctionName: "fn",
		Runtime:      "provided.al2023",
		Handler:      bootstrapEntry,
		Role:         "arn:aws:iam::000000000000:role/test",
		Code:         FunctionCode{ZipFile: zipped},
	}); err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	out, err := svc.InvokeSync(ctx, "fn", []byte(`{"key":"value"}`))
	if err != nil {
		t.Fatalf("InvokeSync: %v", err)
	}

	if !strings.Contains(string(out), `"handled":true`) {
		t.Errorf("response missing handler marker: %s", out)
	}

	if !strings.Contains(string(out), `"key":"value"`) {
		t.Errorf("handler did not receive event: %s", out)
	}
}

// testBootstrapSrc is a minimal AWS Lambda Runtime API client (stdlib only, no
// aws-lambda-go dependency) that echoes each event back wrapped in a marker.
const testBootstrapSrc = `package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
)

func main() {
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation"
	for {
		resp, err := http.Get(base + "/next")
		if err != nil {
			return
		}
		id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		event, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		out := append([]byte(` + "`" + `{"handled":true,"event":` + "`" + `), event...)
		out = append(out, '}')
		_, _ = http.Post(base+"/"+id+"/response", "application/json", bytes.NewReader(out))
	}
}
`

// buildTestBootstrap compiles testBootstrapSrc into a standalone binary and
// returns its path. It uses a throwaway module so no extra main-module
// dependency is introduced.
func buildTestBootstrap(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module kumotestbootstrap\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(testBootstrapSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, bootstrapEntry)

	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".") //nolint:gosec // building a test fixture binary with a controlled path
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test bootstrap: %v\n%s", err, out)
	}

	return bin
}

// zipBootstrap zips the file at binPath as a "bootstrap" entry.
func zipBootstrap(t *testing.T, binPath string) []byte {
	t.Helper()

	data, err := os.ReadFile(binPath) //nolint:gosec // test-controlled fixture path
	if err != nil {
		t.Fatal(err)
	}

	return zipBytes(t, bootstrapEntry, data)
}

// zipBytes builds an in-memory zip containing a single stored (uncompressed)
// entry.
func zipBytes(t *testing.T, name string, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// envMap turns a KEY=VALUE slice into a map for assertions.
func envMap(kv []string) map[string]string {
	m := make(map[string]string, len(kv))

	for _, e := range kv {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}

	return m
}
