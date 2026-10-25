// Command speedtest serves a small download speed test page with
// in-memory generated test files over HTTP and HTTPS (Let's Encrypt).
package main

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

type testFile struct {
	Name  string
	Label string
	Size  int64
}

var files = []testFile{
	{"100MB.bin", "100 MB", 100e6},
	{"1GB.bin", "1 GB", 1e9},
	{"5GB.bin", "5 GB", 5e9},
	{"10GB.bin", "10 GB", 10e9},
	{"100GB.bin", "100 GB", 100e9},
}

// pattern is filled with random bytes once and repeated to make up file contents.
var pattern = make([]byte, 4<<20)

// patternReader is an io.ReadSeeker over size bytes of repeated pattern.
type patternReader struct {
	size, off int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if rem := r.size - r.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	n := 0
	for n < len(p) {
		c := copy(p[n:], pattern[(r.off+int64(n))%int64(len(pattern)):])
		n += c
	}
	r.off += int64(n)
	return n, nil
}

func (r *patternReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.off = offset
	return offset, nil
}

// limiter caps concurrent downloads per client (IPv4 address or IPv6 /64).
type limiter struct {
	mu    sync.Mutex
	max   int
	count map[netip.Prefix]int
}

func clientKey(remoteAddr string) netip.Prefix {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Prefix{}
	}
	a := ap.Addr().Unmap()
	if a.Is4() {
		return netip.PrefixFrom(a, 32)
	}
	p, _ := a.Prefix(64)
	return p
}

func (l *limiter) acquire(k netip.Prefix) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count[k] >= l.max {
		return false
	}
	l.count[k]++
	return true
}

func (l *limiter) release(k netip.Prefix) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count[k]--; l.count[k] <= 0 {
		delete(l.count, k)
	}
}

type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func fileHandler(f testFile, lim *limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", `attachment; filename="`+f.Name+`"`)
		if r.Method == http.MethodHead {
			http.ServeContent(w, r, "", time.Time{}, &patternReader{size: f.Size})
			return
		}

		k := clientKey(r.RemoteAddr)
		if !lim.acquire(k) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many concurrent downloads from your address", http.StatusTooManyRequests)
			log.Printf("%s %s 429 limit reached", k, f.Name)
			return
		}
		defer lim.release(k)

		cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		http.ServeContent(cw, r, "", time.Time{}, &patternReader{size: f.Size})
		d := time.Since(start)
		log.Printf("%s %s %d %d bytes %s %.1f Mbit/s",
			k, f.Name, cw.status, cw.n, d.Round(time.Millisecond), float64(cw.n)*8/d.Seconds()/1e6)
	}
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>wberg networks speedtest</title>
<style>
:root { color-scheme: light dark; --fg: #1a1a1a; --bg: #fafafa; --muted: #666; --link: #0b5cad; --line: #ddd; }
@media (prefers-color-scheme: dark) { :root { --fg: #e6e6e6; --bg: #141414; --muted: #999; --link: #6cb4ff; --line: #333; } }
body { font: 16px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; color: var(--fg); background: var(--bg); margin: 0; padding: 3rem 1rem; }
main { max-width: 40rem; margin: 0 auto; }
h1 { font-size: 1.5rem; margin: 0 0 1rem; }
p { color: var(--muted); }
table { border-collapse: collapse; width: 100%; margin-top: 1.5rem; }
td { padding: .5rem 0; border-bottom: 1px solid var(--line); }
td.num { text-align: right; color: var(--muted); }
a { color: var(--link); text-decoration: none; }
a:hover { text-decoration: underline; }
</style>
</head>
<body>
<main>
<h1>wberg networks speedtest</h1>
<p>Test files for measuring download throughput. Content is incompressible
pseudo-random data and is served over both HTTP and HTTPS. Range requests are
supported. Max {{.PerIP}} concurrent downloads per IP.</p>
<table>
{{range .Files}}<tr><td><a href="/{{.Name}}">{{.Label}}</a></td><td class="num">{{.Size}} bytes</td></tr>
{{end}}</table>
</main>
</body>
</html>
`))

func main() {
	domain := flag.String("domain", "", "comma-separated hostnames for Let's Encrypt; empty runs HTTP only")
	email := flag.String("email", "", "optional ACME contact email")
	cacheDir := flag.String("cache", "/var/lib/speedtest/autocert", "certificate cache directory")
	httpAddr := flag.String("http", ":80", "HTTP listen address")
	httpsAddr := flag.String("https", ":443", "HTTPS listen address")
	perIP := flag.Int("per-ip", 4, "max concurrent downloads per IPv4 address or IPv6 /64")
	flag.Parse()

	if _, err := rand.Read(pattern); err != nil {
		log.Fatal(err)
	}

	lim := &limiter{max: *perIP, count: map[netip.Prefix]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTmpl.Execute(w, struct {
			PerIP int
			Files []testFile
		}{*perIP, files})
	})
	for _, f := range files {
		mux.Handle("GET /"+f.Name, fileHandler(f, lim))
	}

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if *domain == "" {
		log.Printf("no -domain given, serving HTTP only on %s", *httpAddr)
		log.Fatal(httpSrv.ListenAndServe())
	}

	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(strings.Split(*domain, ",")...),
		Cache:      autocert.DirCache(*cacheDir),
		Email:      *email,
	}
	httpSrv.Handler = m.HTTPHandler(mux)

	tlsConf := m.TLSConfig()
	tlsConf.NextProtos = []string{"http/1.1", acme.ALPNProto}
	httpsSrv := &http.Server{
		Addr:              *httpsAddr,
		Handler:           mux,
		TLSConfig:         tlsConf,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // disable HTTP/2
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() { log.Fatal(httpSrv.ListenAndServe()) }()
	log.Printf("serving %s on %s and %s", *domain, *httpAddr, *httpsAddr)
	log.Fatal(httpsSrv.ListenAndServeTLS("", ""))
}
