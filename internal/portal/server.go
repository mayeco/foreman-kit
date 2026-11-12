package portal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// Option configures a Server.
type Option func(*Server)

// WithJudge attaches an explicit evaluator Judge to a Server.
func WithJudge(j *evaluator.Judge) Option {
	return func(s *Server) {
		s.judge = j
	}
}

// WithHub attaches the daemon's pipeline Hub: actions that resume, unblock, approve, reset or queue work
// wake its continuous dispatcher and merger at once instead of on their next poll.
func WithHub(h *orchestrator.Hub) Option {
	return func(s *Server) {
		s.hub = h
	}
}

// WithBind configures the allowed bind hostname/address for Host/Origin validation.
func WithBind(bind string) Option {
	return func(s *Server) {
		s.bind = strings.TrimSpace(bind)
	}
}

// DefaultPort and DefaultBind are where the portal listens when nothing says otherwise: what the kit
// ships for the two settings of the daemon.
var (
	DefaultPort, _ = settings.Of(settings.DaemonPort).Default.(int)
	DefaultBind, _ = settings.Of(settings.DaemonBind).Default.(string)
)

// Server serves the OpenSpec operator portal HTTP API, SSE streams, and server-rendered pages.
type Server struct {
	root    string
	app     string // the name of the workspace: the last element of root
	bind    string
	store   *store.Store
	judge   *evaluator.Judge
	kit     Kit
	hub     *orchestrator.Hub // nil outside the daemon: its notifications are no-ops
	exec    *executions       // what the portal launched (svc_executions.go)
	changes *changes          // what a person decides about a change (svc_changes.go)
	work    *workspace        // what a person does about the kit and its queue (svc_workspace.go)
	tokens  *tokenLimiter     // throttles wrong portal tokens per remote address (token_limiter.go)
	bindMu  sync.Mutex
	// backlogEvery is how often a portal that serves asks the files of the backlog whether they changed.
	backlogEvery time.Duration

	routesOnce sync.Once
	mux        *http.ServeMux // every route of the portal, built on first use (routes.go)
}

// portalExecDrainTimeout bounds how long shutdown waits for canceled runs and jobs to finish; a job
// running agents or gates needs up to proc.DefaultWaitDelay to stop their process groups.
const portalExecDrainTimeout = 75 * time.Second

// New creates the portal Server of the workspace at root, which reads and writes through st and
// starts processes through kit. It needs both: a Server never stands in for the state database or
// for the kit with something that does nothing.
func New(root string, st *store.Store, kit Kit, opts ...Option) (*Server, error) {
	if st == nil {
		return nil, errors.New("portal: a store is required: the pages read the state database")
	}
	if kit == nil {
		return nil, errors.New("portal: a Kit is required: nothing else runs the agents and the operations the portal starts")
	}
	if root == "" {
		root = st.CanonicalRoot()
	}
	if root == "" {
		root = store.AppRoot("")
	}
	s := &Server{
		root:         root,
		app:          filepath.Base(root),
		bind:         DefaultBind,
		store:        st,
		kit:          kit,
		exec:         newExecutions(root, st, kit),
		tokens:       newTokenLimiter(),
		backlogEvery: 5 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.judge == nil {
		// A workspace whose evaluator is not configured has no judge: reviewing proposals then says so.
		s.judge, _ = evaluator.NewJudge(root, st)
	}
	// After the options: the services tell the pipeline of the daemon, when the portal runs in one.
	s.changes = &changes{root: root, store: st, hub: s.hub}
	s.work = &workspace{root: root, store: st, hub: s.hub, kit: kit, judge: s.judge}
	return s, nil
}

// syncBacklog brings the mirror of the backlog and the proposals in the database up to date with
// their files. Who writes the files owes the mirror: the dispatcher, the merger and the cycle do it
// as they work, and the portal after its own edits. A page never does: reading writes nothing.
func (s *Server) syncBacklog(ctx context.Context) {
	if err := s.store.SyncJSONToSQLite(ctx, s.root); err != nil && ctx.Err() == nil {
		log.Printf("portal: bringing the mirror of the backlog up to date: %v", err)
	}
}

// mirrorBacklog brings the mirror of the backlog up to date as the portal starts to serve, and
// keeps it so until ctx ends.
func (s *Server) mirrorBacklog(ctx context.Context) {
	// The signature is taken before the files are read: a write that lands in between is seen by
	// the first round of the watch.
	seen := s.store.MirroredFilesSignature(s.root)
	s.syncBacklog(ctx)
	go s.watchBacklog(ctx, seen)
}

// watchBacklog mirrors the files of the backlog again whenever one of them has changed since seen,
// until ctx ends. The kit mirrors what it writes; this is for the edit nobody announces: the editor
// of a person, a git pull. Each round asks the files for their size and their time, and reads them
// only when one has moved.
func (s *Server) watchBacklog(ctx context.Context, seen string) {
	tick := time.NewTicker(s.backlogEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if ctx.Err() != nil {
				return
			}
			if now := s.store.MirroredFilesSignature(s.root); now != seen {
				seen = now
				s.syncBacklog(ctx)
			}
		}
	}
}

func (s *Server) Root() string        { return s.root }
func (s *Server) Store() *store.Store { return s.store }

func (s *Server) setBind(bind string) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if strings.TrimSpace(bind) != "" {
		s.bind = strings.TrimSpace(bind)
	}
}

func (s *Server) getBind() string {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	return s.bind
}

func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(h, "[]")
}

func isLoopbackHost(h string) bool {
	host := normalizeHost(h)
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func normalizeOriginHostPort(hostPort, scheme string) string {
	hostPort = strings.TrimSpace(strings.ToLower(hostPort))
	if hostPort == "" {
		return ""
	}
	var host, port string
	if h, p, err := net.SplitHostPort(hostPort); err == nil {
		host = strings.Trim(h, "[]")
		port = strings.TrimSpace(p)
	} else {
		host = strings.Trim(hostPort, "[]")
	}
	if host == "" {
		return ""
	}
	if port == "" {
		if strings.EqualFold(strings.TrimSpace(scheme), "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	if isLoopbackHost(host) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (s *Server) isAllowedHost(reqHost string) bool {
	if reqHost == "" {
		return true
	}
	if isLoopbackHost(reqHost) {
		return true
	}
	bindHost := normalizeHost(s.getBind())
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" && reqHost == bindHost {
		return true
	}
	if (bindHost == "0.0.0.0" || bindHost == "::") && net.ParseIP(reqHost) != nil {
		return true
	}
	return false
}

func (s *Server) isAllowedOrigin(originHostPort, reqHostPort string) bool {
	if originHostPort == "" || reqHostPort == "" {
		return false
	}
	return originHostPort == reqHostPort
}

func (s *Server) requiresNonLoopbackAuth(r *http.Request) bool {
	if bind := s.getBind(); bind != "" && !isLoopbackHost(bind) {
		return true
	}
	if r != nil && strings.TrimSpace(r.RemoteAddr) != "" && !isLoopbackHost(r.RemoteAddr) {
		return true
	}
	return false
}

func providedPortalToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHdr) > 7 && strings.EqualFold(authHdr[:7], "Bearer ") {
		if provided := strings.TrimSpace(authHdr[7:]); provided != "" {
			return provided
		}
	}
	return strings.TrimSpace(r.Header.Get("X-OpenSpec-Token"))
}

func matchesPortalToken(provided string) bool {
	expected := strings.TrimSpace(os.Getenv("FOREMANKIT_PORTAL_TOKEN"))
	if expected == "" || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func hasValidPortalToken(r *http.Request) bool {
	return matchesPortalToken(providedPortalToken(r))
}

func (s *Server) validateAPIRequest(w http.ResponseWriter, r *http.Request, path string) bool {
	reqHost := normalizeHost(r.Host)
	if !s.isAllowedHost(reqHost) {
		fail(w, r, errForbidden("the portal does not answer to the host %q", reqHost))
		return false
	}
	if s.requiresNonLoopbackAuth(r) {
		ip := remoteIP(r)
		if retryAfter, ok := s.tokens.locked(ip); ok {
			w.Header().Set("Retry-After", retryAfterHeader(retryAfter))
			fail(w, r, errRateLimited("too many wrong tokens from this address; try again later"))
			return false
		}
		provided := providedPortalToken(r)
		if !matchesPortalToken(provided) {
			if provided != "" {
				s.tokens.fail(ip)
			}
			fail(w, r, errForbidden("a request from outside this machine needs the token of the portal"))
			return false
		}
	}

	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			fail(w, r, errForbidden("an action is not taken from another site"))
			return false
		}
		reqScheme := "http"
		if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			reqScheme = "https"
		}
		reqHostPort := normalizeOriginHostPort(r.Host, reqScheme)
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme == "" || u.Host == "" {
				fail(w, r, errForbidden("an action is not taken from another origin"))
				return false
			}
			originHostPort := normalizeOriginHostPort(u.Host, u.Scheme)
			if !s.isAllowedOrigin(originHostPort, reqHostPort) {
				fail(w, r, errForbidden("an action is not taken from another origin"))
				return false
			}
		} else if origin == "null" {
			fail(w, r, errForbidden("an action is not taken from another origin"))
			return false
		} else if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
			u, err := url.Parse(referer)
			if err != nil || u.Scheme == "" || u.Host == "" {
				fail(w, r, errForbidden("an action is not taken from another origin"))
				return false
			}
			refHostPort := normalizeOriginHostPort(u.Host, u.Scheme)
			if !s.isAllowedOrigin(refHostPort, reqHostPort) {
				fail(w, r, errForbidden("an action is not taken from another origin"))
				return false
			}
		}
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		isJSON := strings.Contains(ct, "application/json")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") ||
			strings.HasPrefix(ct, "multipart/form-data") ||
			strings.HasPrefix(ct, "text/plain") {
			fail(w, r, errForbidden("an action takes its body as application/json, and nothing in its address"))
			return false
		}
		// An action whose body is not declared as JSON carries nothing in its address either. The
		// actions of the redesigned API read their input from a JSON body alone (decodeInput).
		if !isJSON && r.URL.RawQuery != "" {
			fail(w, r, errForbidden("an action takes its body as application/json, and nothing in its address"))
			return false
		}
	}
	return true
}

// maxRequestBody bounds the body of a request to the API: an action takes a few fields, never a file.
const maxRequestBody = 1 << 20

// ServeHTTP answers a request through the one router of the portal (routes.go), after the checks
// every request gets: the host and the token, the origin of an action, and the canonical address.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := r.URL.Path
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}

	switch {
	case path == "/api" || strings.HasPrefix(path, "/api/"):
		if !s.validateAPIRequest(w, r, path) {
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		// The API takes an address with a trailing slash as the address without it. Its root alone
		// keeps the slash: it is the address the mux gives the routes nobody declared.
		if path == "/api" {
			path = "/api/"
		}
		r = withPath(r, path)
		if !muxTakesAPIMethod(r.Method) {
			s.apiNoRoute(w, r)
			return
		}
	case strings.HasPrefix(path, "/assets/"):
		r = withPath(r, path)
	default:
		// A page has one address: the one without a trailing slash.
		if path != r.URL.Path {
			// The canonical address must be a page of this portal: //host is a protocol-relative URL that
			// would send the browser to another site, and so are /\host and /<TAB>/host, since browsers read
			// a backslash as a slash and drop tabs and newlines from a Location. The target keeps the
			// request's percent-encoding (a%2Fb stays one segment).
			if path == "" || strings.HasPrefix(path, "//") || strings.IndexFunc(path, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) >= 0 {
				http.NotFound(w, r)
				return
			}
			target := strings.TrimRight(r.URL.EscapedPath(), "/")
			if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
				http.NotFound(w, r)
				return
			}
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		if !s.validatePageRequest(w, r) {
			return
		}
	}
	s.router().ServeHTTP(w, r)
}

// withPath returns r with path as its address: the address it came with, give or take the slashes
// at its end. The percent-encoding of the address is kept (a%2Fb stays one segment).
func withPath(r *http.Request, path string) *http.Request {
	if path == r.URL.Path {
		return r
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	if raw := strings.TrimRight(r.URL.RawPath, "/"); raw != "" && strings.HasSuffix(path, "/") {
		r2.URL.RawPath = raw + "/"
	} else {
		r2.URL.RawPath = raw
	}
	return r2
}

// httpServer returns the HTTP server of the portal. It bounds how long a client may take to send its
// headers and how long an idle connection is kept; it does not bound a response, because the log of
// an execution is streamed for as long as the execution runs.
func (s *Server) httpServer() *http.Server {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	srv.RegisterOnShutdown(s.exec.beginShutdown)
	return srv
}

// ListenAndServe starts the HTTP server on addr until ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if addr == "" {
		addr = fmt.Sprintf("%s:%d", DefaultBind, DefaultPort)
	}
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" {
		s.setBind(h)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// The pages read the mirror of the backlog and never bring it up to date: the portal does that
	// here, as it starts, after each of its own edits, and when it sees the files change.
	s.mirrorBacklog(ctx)
	srv := s.httpServer()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		s.exec.beginShutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Serve returns as soon as Shutdown starts; wait for Shutdown itself so in-flight handlers finish
	// before the caller closes the store.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// The handlers that did not finish in time lose their connections now; what Close returns
		// adds nothing to the error Shutdown already gave.
		_ = srv.Close()
	}
	// Shutdown canceled portal-launched runs and jobs (beginShutdown); let them stop their process
	// groups before the caller closes the store and the process exits.
	s.exec.wait(portalExecDrainTimeout)
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// StartBackground binds the portal HTTP server in a background goroutine (probing up to 50 ports
// if preferredPort is busy and explicitPort is false) and returns the bound http:// URL and Shutdown func.
func (s *Server) StartBackground(ctx context.Context, bind string, preferredPort int, explicitPort bool) (string, func(context.Context) error, error) {
	if bind == "" {
		bind = DefaultBind
	}
	s.setBind(bind)
	if preferredPort <= 0 {
		preferredPort = DefaultPort
	}
	maxTries := 50
	if explicitPort {
		maxTries = 1
	}
	var ln net.Listener
	var err error
	for i := 0; i < maxTries; i++ {
		addr := fmt.Sprintf("%s:%d", bind, preferredPort+i)
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", nil, err
	}
	// As in ListenAndServe: the mirror of the backlog is up to date before the first page is served,
	// and follows the files until the portal stops.
	watchCtx, stopWatch := context.WithCancel(ctx)
	s.mirrorBacklog(watchCtx)
	httpSrv := s.httpServer()
	go func() {
		// Serve returns when the server is shut down: that is how it ends, not an error to report.
		_ = httpSrv.Serve(ln)
	}()
	url := fmt.Sprintf("http://%s", ln.Addr().String())
	shutdown := func(sdCtx context.Context) error {
		stopWatch()
		err := httpSrv.Shutdown(sdCtx)
		if err != nil {
			// Handlers still running after the grace period lose their connections now,
			// before the daemon closes the store under them.
			_ = httpSrv.Close()
		}
		s.exec.wait(portalExecDrainTimeout)
		return err
	}
	return url, shutdown, nil
}

// listenWhere reads where the portal of the application at root listens when its command line does
// not say: the port and the address the settings of the daemon give, which is where the portal a
// daemon serves listens too. A key its file lacks, or holds wrong, does not keep the portal from
// starting, since the portal is where a person reads what is wrong with the file: it listens where
// the kit ships, and says so.
func listenWhere(root string, stderr io.Writer) (port int, bind string) {
	r := &settings.Reader{Root: root, Stderr: io.Discard}
	port, bind = DefaultPort, DefaultBind
	if d := r.Describe(settings.DaemonPort); d.InEffect != "" {
		if p, err := strconv.Atoi(d.InEffect); err == nil && p > 0 {
			port = p
		}
	} else {
		say(stderr, "portal: %s is %s: listening on the port the kit ships, %d\n", settings.DaemonPort.Where(), d.Problem, port)
	}
	if d := r.Describe(settings.DaemonBind); d.InEffect != "" {
		bind = d.InEffect
	} else {
		say(stderr, "portal: %s is %s: listening on the address the kit ships, %s\n", settings.DaemonBind.Where(), d.Problem, bind)
	}
	return port, bind
}

// say writes a line of what the command tells the person who ran it. When the terminal or the pipe
// takes no more, nowhere is left to say so: that error is dropped here, once, for every line.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) // nowhere is left to report that the output is gone
}

// RunPortalCLI implements `foreman-kit portal [--port 4567] [--bind 127.0.0.1]`. kit runs what the
// portal starts (NewKit in a real installation); opts configure the Server. Without --port or
// --bind it listens where the settings of the application say: nothing is read from the
// environment, where a variable called PORT is what a gate gives the service it starts.
func RunPortalCLI(ctx context.Context, root string, st *store.Store, kit Kit, args []string, stdout, stderr io.Writer, opts ...Option) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	port, bind := listenWhere(root, stderr)

	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-p" || args[i] == "--port") && i+1 < len(args):
			if p, err := strconv.Atoi(args[i+1]); err == nil && p > 0 {
				port = p
			}
			i++
		case (args[i] == "-o" || args[i] == "--bind") && i+1 < len(args):
			bind = args[i+1]
			i++
		default:
			say(stderr, "Usage: foreman-kit portal [--port 4567] [--bind 127.0.0.1]\n")
			return 2
		}
	}

	addr := fmt.Sprintf("%s:%d", bind, port)
	srv, err := New(root, st, kit, opts...)
	if err != nil {
		say(stderr, "portal: %v\n", err)
		return 1
	}
	say(stdout, "portal: listening on http://%s\n", addr)
	if err := srv.ListenAndServe(ctx, addr); err != nil {
		say(stderr, "portal: %v\n", err)
		return 1
	}
	return 0
}

// writeJSON answers with data as JSON. What cannot be written as JSON is a failure of the portal:
// it is answered as one, and goes to the log, instead of an empty answer under the status of a
// success.
func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("portal: an answer could not be written as JSON: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error": {"code": "internal", "message": "The answer could not be written as JSON."}}`) // a client that went away is not an error to report
		return
	}
	w.WriteHeader(code)
	_, _ = w.Write(b) // a client that went away is not an error to report
}
