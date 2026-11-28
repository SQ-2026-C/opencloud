package middleware

// Pruebas de verificación a nivel de componente de la cadena de autenticación
// del proxy (SQ 2026-II, Entrega 1, equipo C, componente #4).
//
// Cada subtest lleva el ID del caso documentado en
// p1/componente-4-authentication.md. Convención de IDs:
//   BY = bypass (rutas sin autenticación), AU = tabla de decisión de la cadena,
//   TF = fallo terminal, CH = challenges Www-Authenticate, WD = cuerpo WebDAV
//   y drenado del body, CF = configuración global y trazas.
//
// Ejecutar:  go test ./services/proxy/pkg/middleware/ -run 'TestContractAuth' -v

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/proxy/pkg/config"
	"github.com/opencloud-eu/opencloud/services/proxy/pkg/router"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ---------------------------------------------------------------------------
// Dobles de prueba
// ---------------------------------------------------------------------------

type identityKey struct{}

// fakeAuthenticator es un stub programable (devuelve el resultado indicado)
// y un spy (cuenta llamadas y registra el orden en que se invoca). Si el
// resultado es Succeeded, aumenta la petición con una "identidad" en el
// contexto, como hacen los autenticadores reales.
type fakeAuthenticator struct {
	name   string
	result AuthenticationResult
	calls  int
	order  *[]string
}

func (f *fakeAuthenticator) Authenticate(r *http.Request) AuthenticationResult {
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, f.name)
	}
	res := f.result
	if res.State == AuthenticationSucceeded {
		res.Request = r.WithContext(context.WithValue(r.Context(), identityKey{}, f.name))
	}
	return res
}

// suppressingAuthenticator además implementa authenticationChallengeSuppressor,
// como SignedURLAuthenticator y GuestLinkAuthenticator.
type suppressingAuthenticator struct {
	fakeAuthenticator
	suppress bool
	asked    int
}

func (s *suppressingAuthenticator) SuppressAuthenticationChallenge(*http.Request) bool {
	s.asked++
	return s.suppress
}

func fake(name string, res AuthenticationResult) *fakeAuthenticator {
	return &fakeAuthenticator{name: name, result: res}
}

// otherDetails es un ErrorDetails desconocido para renderErrorDetails.
type otherDetails struct{}

func (otherDetails) isErrorDetails() {}

// failingWriter simula un cliente que cerró la conexión: Write falla.
type failingWriter struct{ *httptest.ResponseRecorder }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// spyBody registra cuánto se leyó del body y si se cerró.
type spyBody struct {
	r      io.Reader
	read   int
	closed bool
}

func (b *spyBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}
func (b *spyBody) Close() error { b.closed = true; return nil }

// routingInfo obtiene un RoutingInfo real del componente #5 (sus campos no
// son exportados). Es un fixture determinista: una única ruta "/".
func routingInfo(unprotected bool) router.RoutingInfo {
	rt := router.New(nil, &config.PolicySelector{Static: &config.StaticSelectorConf{Policy: "p"}},
		[]config.Policy{{Name: "p", Routes: []config.Route{{Endpoint: "/", Backend: "http://backend", Unprotected: unprotected}}}},
		log.NopLogger())
	ri, _ := rt.Route(httptest.NewRequest(http.MethodGet, "/", nil))
	return ri
}

func newReq(method, target string, unprotected bool) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(router.SetRoutingInfo(r.Context(), routingInfo(unprotected)))
}

// resetStrategies aísla la variable global SupportedAuthStrategies, que
// Authentication() modifica cada vez que se construye.
func resetStrategies(t *testing.T) {
	t.Helper()
	prev := SupportedAuthStrategies
	SupportedAuthStrategies = nil
	t.Cleanup(func() { SupportedAuthStrategies = prev })
}

type outcome struct {
	rec        *httptest.ResponseRecorder
	nextCalled bool
	identity   any
}

func (o outcome) challenges() []string { return o.rec.Header().Values(WwwAuthenticate) }

func serve(t *testing.T, auths []Authenticator, r *http.Request, opts ...Option) outcome {
	t.Helper()
	resetStrategies(t)
	var o outcome
	h := Authentication(auths, opts...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.nextCalled = true
		o.identity = r.Context().Value(identityKey{})
	}))
	o.rec = httptest.NewRecorder()
	h.ServeHTTP(o.rec, r)
	return o
}

func spyLog(t *testing.T) (log.Logger, *bytes.Buffer) {
	t.Helper()
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	var buf bytes.Buffer
	return log.Logger{Logger: zerolog.New(&buf)}, &buf
}

const (
	bearerCh = `Bearer realm="example.com", charset="UTF-8"`
	basicCh  = `Basic realm="example.com", charset="UTF-8"`
)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// BY — peticiones que no pasan por la cadena
// ---------------------------------------------------------------------------

func TestContractAuthBypass(t *testing.T) {
	cases := []struct {
		id, method, target string
		unprotected        bool
		wantBypass         bool
	}{
		{"BY-01 ruta sin protección", http.MethodGet, "/status.php", true, true},
		{"BY-02 OPTIONS en ruta protegida", http.MethodOptions, "/graph/v1.0/users", false, true},
		{"BY-03 endpoint de token OIDC", http.MethodPost, "/konnect/v1/token", false, true},
		{"BY-04 límite: token OIDC con '/' final sí se autentica", http.MethodPost, "/konnect/v1/token/", false, false},
		{"BY-05 ruta protegida pasa por la cadena", http.MethodGet, "/graph/v1.0/me", false, false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			a := fake("a", Failed())
			o := serve(t, []Authenticator{a}, newReq(c.method, c.target, c.unprotected))
			if c.wantBypass {
				if !o.nextCalled || a.calls != 0 || o.rec.Code != http.StatusOK {
					t.Errorf("esperado bypass: next=%v llamadas=%d code=%d", o.nextCalled, a.calls, o.rec.Code)
				}
				if o.identity != nil {
					t.Errorf("en bypass no debe haber identidad, hay %v", o.identity)
				}
				return
			}
			if o.nextCalled || a.calls != 1 || o.rec.Code != http.StatusUnauthorized {
				t.Errorf("esperado paso por la cadena y 401: next=%v llamadas=%d code=%d", o.nextCalled, a.calls, o.rec.Code)
			}
		})
	}

	t.Run("BY-06 sin RoutingInfo en el contexto: pánico (precondición)", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Errorf("se esperaba pánico")
			}
		}()
		serve(t, nil, httptest.NewRequest(http.MethodGet, "/graph", nil))
	})
}

// ---------------------------------------------------------------------------
// AU — tabla de decisión sobre la secuencia de resultados de la cadena
// ---------------------------------------------------------------------------

func TestContractAuthChainDecisionTable(t *testing.T) {
	errDown := errors.New("backend caído")
	terminalNA := AuthenticationResult{State: AuthenticationNotApplicable, Terminal: true}
	terminalErr := AuthenticationResult{State: AuthenticationError, Err: errDown, Terminal: true}

	cases := []struct {
		id         string
		results    []AuthenticationResult
		wantNext   bool
		wantWho    string // autenticador cuya identidad llega al siguiente handler
		wantCalled []string
		wantCode   int
		wantCh     bool // ¿se esperan challenges?
	}{
		{"AU-01 primero Succeeded", []AuthenticationResult{Succeeded(nil), Succeeded(nil)}, true, "a0", []string{"a0"}, 200, false},
		{"AU-02 NotApplicable y luego Succeeded", []AuthenticationResult{NotApplicable(), Succeeded(nil)}, true, "a1", []string{"a0", "a1"}, 200, false},
		{"AU-03 Failed no terminal no corta la cadena", []AuthenticationResult{FailedWithErr(errors.New("clave")), Succeeded(nil)}, true, "a1", []string{"a0", "a1"}, 200, false},
		{"AU-04 Error no terminal no corta la cadena", []AuthenticationResult{AuthenticationErrorResult(errDown), Succeeded(nil)}, true, "a1", []string{"a0", "a1"}, 200, false},
		{"AU-05 todos NotApplicable: 401 con challenge", []AuthenticationResult{NotApplicable(), NotApplicable()}, false, "", []string{"a0", "a1"}, 401, true},
		{"AU-06 todos Failed: 401 con challenge", []AuthenticationResult{Failed(), FailedWithErr(errors.New("x"))}, false, "", []string{"a0", "a1"}, 401, true},
		{"AU-07 TerminalFailed corta: el siguiente no se prueba", []AuthenticationResult{TerminalFailed(), Succeeded(nil)}, false, "", []string{"a0"}, 401, false},
		{"AU-08 Failed, luego TerminalFailedWithErr, luego Succeeded", []AuthenticationResult{Failed(), TerminalFailedWithErr(errors.New("revocado")), Succeeded(nil)}, false, "", []string{"a0", "a1"}, 401, false},
		{"AU-09 cadena vacía: 401 con challenge", nil, false, "", nil, 401, true},
		{"AU-10 Error no terminal al final: 401 con challenge (igual que Failed)", []AuthenticationResult{NotApplicable(), AuthenticationErrorResult(errDown)}, false, "", []string{"a0", "a1"}, 401, true},
		{"AU-11 Terminal con estado NotApplicable: corta pero responde como no terminal", []AuthenticationResult{terminalNA, Succeeded(nil)}, false, "", []string{"a0"}, 401, true},
		{"AU-12 Terminal con estado Error: respuesta terminal sin challenge", []AuthenticationResult{terminalErr, Succeeded(nil)}, false, "", []string{"a0"}, 401, false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			var order []string
			auths := make([]Authenticator, 0, len(c.results))
			for i, res := range c.results {
				f := fake("a"+string(rune('0'+i)), res)
				f.order = &order
				auths = append(auths, f)
			}
			o := serve(t, auths, newReq(http.MethodGet, "/graph/v1.0/me", false), OIDCIss("https://idp"))
			if o.nextCalled != c.wantNext || o.rec.Code != c.wantCode {
				t.Errorf("next=%v code=%d, esperado next=%v code=%d", o.nextCalled, o.rec.Code, c.wantNext, c.wantCode)
			}
			if c.wantNext && o.identity != c.wantWho {
				t.Errorf("identidad = %v, esperado %q (debe llegar la petición aumentada)", o.identity, c.wantWho)
			}
			if !equalStrings(order, c.wantCalled) {
				t.Errorf("autenticadores llamados = %v, esperado %v", order, c.wantCalled)
			}
			if got := len(o.challenges()) > 0; got != c.wantCh {
				t.Errorf("challenges = %v, esperado presentes=%v", o.challenges(), c.wantCh)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TF — respuesta de los fallos terminales
// ---------------------------------------------------------------------------

func TestContractAuthTerminalFailure(t *testing.T) {
	expired := AuthenticationResult{
		State: AuthenticationFailed, Terminal: true, CookiesToClear: []string{"oc_guest", "oc_guest2"},
		ErrorDetails: GuestSessionExpiredDetails{PermissionID: "perm-1"},
	}

	t.Run("TF-01 genérico no WebDAV: 401 sin cuerpo ni challenge", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", TerminalFailed())}, newReq(http.MethodGet, "/graph/v1.0/me", false), OIDCIss("x"))
		if o.rec.Code != 401 || o.rec.Body.Len() != 0 || len(o.challenges()) != 0 {
			t.Errorf("code=%d body=%q ch=%v", o.rec.Code, o.rec.Body.String(), o.challenges())
		}
	})

	t.Run("TF-02 genérico WebDAV: XML NotAuthenticated", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", TerminalFailed())}, newReq("PROPFIND", "/remote.php/dav/files/u", false))
		if o.rec.Code != 401 || !strings.HasPrefix(o.rec.Header().Get("Content-Type"), "application/xml") ||
			!strings.Contains(o.rec.Body.String(), `Sabre\DAV\Exception\NotAuthenticated`) {
			t.Errorf("code=%d ct=%q body=%s", o.rec.Code, o.rec.Header().Get("Content-Type"), o.rec.Body.String())
		}
	})

	t.Run("TF-03 sesión de invitado expirada, no WebDAV: JSON y borra cookies", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", expired)}, newReq(http.MethodGet, "/s/abc", false))
		body := o.rec.Body.String()
		if o.rec.Code != 401 || o.rec.Header().Get("Content-Type") != "application/json" ||
			!strings.Contains(body, `"error_type":"session_expired"`) || !strings.Contains(body, `"permissionId":"perm-1"`) {
			t.Errorf("code=%d ct=%q body=%s", o.rec.Code, o.rec.Header().Get("Content-Type"), body)
		}
		cleared := map[string]bool{}
		for _, ck := range o.rec.Result().Cookies() {
			if ck.MaxAge < 0 || ck.Value == "" {
				cleared[ck.Name] = true
			}
		}
		if !cleared["oc_guest"] || !cleared["oc_guest2"] {
			t.Errorf("cookies borradas = %v, esperado oc_guest y oc_guest2", cleared)
		}
	})

	t.Run("TF-04 sesión de invitado expirada, WebDAV: XML con share_id", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", expired)}, newReq("PROPFIND", "/dav/public-files/abc", false))
		if o.rec.Code != 401 || !strings.Contains(o.rec.Body.String(), "<opencloud:share_id>perm-1</opencloud:share_id>") {
			t.Errorf("code=%d body=%s", o.rec.Code, o.rec.Body.String())
		}
	})

	t.Run("TF-05 ErrorDetails desconocido: 401 sin cuerpo", func(t *testing.T) {
		res := AuthenticationResult{State: AuthenticationFailed, Terminal: true, ErrorDetails: otherDetails{}}
		o := serve(t, []Authenticator{fake("a", res)}, newReq(http.MethodGet, "/x", false))
		if o.rec.Code != 401 || o.rec.Body.Len() != 0 {
			t.Errorf("code=%d body=%q", o.rec.Code, o.rec.Body.String())
		}
	})

	t.Run("TF-06 falla el render: se registra el error", func(t *testing.T) {
		resetStrategies(t)
		l, buf := spyLog(t)
		h := Authentication([]Authenticator{fake("a", expired)}, Logger(l))(http.NotFoundHandler())
		w := failingWriter{httptest.NewRecorder()}
		h.ServeHTTP(w, newReq(http.MethodGet, "/s/abc", false))
		if !strings.Contains(buf.String(), "Failed to render terminal failure response") {
			t.Errorf("log = %q", buf.String())
		}
		// El código observado es 401, no 500: ver el defecto D2 en authentication_defects_test.go.
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code = %d", w.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// CH — challenges Www-Authenticate en el 401 no terminal
// ---------------------------------------------------------------------------

func TestContractAuthChallenges(t *testing.T) {
	na := func() []Authenticator { return []Authenticator{fake("a", NotApplicable())} }
	ua := func(r *http.Request, v string) *http.Request { r.Header.Set("User-Agent", v); return r }
	xhr := func(r *http.Request) *http.Request { r.Header.Set("X-Requested-With", "XMLHttpRequest"); return r }
	locks := CredentialsByUserAgent(map[string]string{"mirall": "basic"})

	cases := []struct {
		id   string
		r    *http.Request
		opts []Option
		want []string
	}{
		{"CH-01 solo OIDC: Bearer", newReq("GET", "/graph/me", false), []Option{OIDCIss("x")}, []string{bearerCh}},
		{"CH-02 solo Basic", newReq("GET", "/graph/me", false), []Option{EnableBasicAuth(true)}, []string{basicCh}},
		{"CH-03 ambos, en orden Bearer, Basic", newReq("GET", "/graph/me", false), []Option{OIDCIss("x"), EnableBasicAuth(true)}, []string{bearerCh, basicCh}},
		{"CH-04 ninguna estrategia: sin challenge", newReq("GET", "/graph/me", false), nil, nil},
		{"CH-05 petición XHR: sin challenge", xhr(newReq("GET", "/graph/me", false)), []Option{OIDCIss("x")}, nil},
		{"CH-06 ruta pública: sin challenge", newReq("GET", "/remote.php/dav/public-files/tok", false), []Option{OIDCIss("x")}, nil},
		{"CH-07 límite: casi pública (sin '/' final) sí lleva challenge", newReq("GET", "/remote.php/dav/public-files", false), []Option{OIDCIss("x")}, []string{bearerCh}},
		{"CH-10 User-Agent igual a la clave: solo el challenge fijado", ua(newReq("GET", "/graph/me", false), "mirall"), []Option{OIDCIss("x"), EnableBasicAuth(true), locks}, []string{basicCh}},
		{"CH-11 User-Agent real que contiene la clave: NO se fija (todos)", ua(newReq("GET", "/graph/me", false), "Mozilla/5.0 (Linux) mirall/2.7.1"), []Option{OIDCIss("x"), EnableBasicAuth(true), locks}, []string{bearerCh, basicCh}},
		{"CH-12 User-Agent que es subcadena de la clave: se fija", ua(newReq("GET", "/graph/me", false), "mir"), []Option{OIDCIss("x"), EnableBasicAuth(true), locks}, []string{basicCh}},
		{"CH-13 ruta /ocs/…/cloud/ con lock: un único challenge", ua(newReq("GET", "/ocs/v2.php/cloud/users", false), "mirall"), []Option{OIDCIss("x"), EnableBasicAuth(true), locks}, []string{basicCh}},
		{"CH-14 ruta /ocs/…/cloud/ sin lock que coincida: todos", ua(newReq("GET", "/ocs/v2.php/cloud/users", false), "curl/8"), []Option{OIDCIss("x"), EnableBasicAuth(true), locks}, []string{bearerCh, basicCh}},
		{"CH-15 ruta pública /ocs/…/cloud/ con lock: aparece el challenge", ua(newReq("GET", "/ocs/v1.php/cloud/capabilities", false), "mirall"), []Option{OIDCIss("x"), locks}, []string{basicCh}},
		{"CH-16 XHR con lock: aparece el challenge", ua(xhr(newReq("GET", "/graph/me", false)), "mirall"), []Option{OIDCIss("x"), locks}, []string{basicCh}},
		{"CH-17 ruta pública con '..' hacia una protegida: sin challenge", newReq("GET", "/ocm/../graph/v1.0/me", false), []Option{OIDCIss("x")}, nil},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			o := serve(t, na(), c.r, c.opts...)
			if o.rec.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, esperado 401", o.rec.Code)
			}
			if !equalStrings(o.challenges(), c.want) {
				t.Errorf("Www-Authenticate = %q, esperado %q", o.challenges(), c.want)
			}
		})
	}

	t.Run("CH-08 un supresor activo quita los challenges (y el lock-in)", func(t *testing.T) {
		s := &suppressingAuthenticator{fakeAuthenticator: *fake("signed", NotApplicable()), suppress: true}
		r := ua(newReq("GET", "/ocs/v2.php/cloud/users", false), "mirall")
		o := serve(t, []Authenticator{s, fake("oidc", Failed())}, r, OIDCIss("x"), locks)
		if o.rec.Code != 401 || len(o.challenges()) != 0 || s.asked != 1 {
			t.Errorf("code=%d ch=%v consultas=%d", o.rec.Code, o.challenges(), s.asked)
		}
	})

	t.Run("CH-09 supresor inactivo: challenges normales", func(t *testing.T) {
		s := &suppressingAuthenticator{fakeAuthenticator: *fake("signed", NotApplicable()), suppress: false}
		o := serve(t, []Authenticator{s}, newReq("GET", "/graph/me", false), OIDCIss("x"))
		if !equalStrings(o.challenges(), []string{bearerCh}) || s.asked != 1 {
			t.Errorf("ch=%v consultas=%d", o.challenges(), s.asked)
		}
	})

	t.Run("CH-18 supresor que tuvo éxito no se consulta", func(t *testing.T) {
		s := &suppressingAuthenticator{fakeAuthenticator: *fake("signed", Succeeded(nil)), suppress: true}
		o := serve(t, []Authenticator{s}, newReq("GET", "/graph/me", false))
		if !o.nextCalled || s.asked != 0 {
			t.Errorf("next=%v consultas=%d", o.nextCalled, s.asked)
		}
	})
}

// ---------------------------------------------------------------------------
// WD — cuerpo WebDAV del 401 no terminal y drenado del body
// ---------------------------------------------------------------------------

func TestContractAuthWebdavAndBody(t *testing.T) {
	t.Run("WD-01 PROPFIND no terminal: XML (con código PermissionDenied)", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", Failed())}, newReq("PROPFIND", "/remote.php/dav/files/u", false))
		if o.rec.Code != 401 || !strings.Contains(o.rec.Body.String(), `Sabre\DAV\Exception\PermissionDenied`) {
			t.Errorf("code=%d body=%s", o.rec.Code, o.rec.Body.String())
		}
	})

	t.Run("WD-02 GET no terminal: sin cuerpo", func(t *testing.T) {
		o := serve(t, []Authenticator{fake("a", Failed())}, newReq("GET", "/remote.php/dav/files/u", false))
		if o.rec.Code != 401 || o.rec.Body.Len() != 0 {
			t.Errorf("code=%d body=%q", o.rec.Code, o.rec.Body.String())
		}
	})

	t.Run("WD-03 HTTP/1.x: el body se descarta y se cierra", func(t *testing.T) {
		body := &spyBody{r: strings.NewReader(strings.Repeat("x", 4096))}
		r := newReq("PUT", "/remote.php/dav/files/u/f.txt", false)
		r.Body = body
		serve(t, []Authenticator{fake("a", Failed())}, r)
		if body.read != 4096 || !body.closed {
			t.Errorf("leídos=%d cerrado=%v", body.read, body.closed)
		}
	})

	t.Run("WD-04 HTTP/2: el body no se toca", func(t *testing.T) {
		body := &spyBody{r: strings.NewReader("datos")}
		r := newReq("PUT", "/remote.php/dav/files/u/f.txt", false)
		r.Body, r.ProtoMajor = body, 2
		serve(t, []Authenticator{fake("a", Failed())}, r)
		if body.read != 0 || body.closed {
			t.Errorf("leídos=%d cerrado=%v", body.read, body.closed)
		}
	})
}

// ---------------------------------------------------------------------------
// CF — configuración global y trazas
// ---------------------------------------------------------------------------

func TestContractAuthConfigAndTracing(t *testing.T) {
	t.Run("CF-01 cada construcción vuelve a agregar las estrategias (estado global)", func(t *testing.T) {
		resetStrategies(t)
		Authentication(nil, OIDCIss("x"), EnableBasicAuth(true))
		Authentication(nil, OIDCIss("x"), EnableBasicAuth(true))
		if !equalStrings(SupportedAuthStrategies, []string{"bearer", "basic", "bearer", "basic"}) {
			t.Errorf("SupportedAuthStrategies = %v", SupportedAuthStrategies)
		}
	})

	t.Run("CF-02 bypass: span con routeunprotected=true", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		serve(t, nil, newReq("GET", "/status.php", true), TraceProvider(tp))
		spans := sr.Ended()
		if len(spans) != 1 || spans[0].Name() != "GET /status.php" {
			t.Fatalf("spans = %v", spans)
		}
		found := false
		for _, kv := range spans[0].Attributes() {
			if kv == attribute.Bool("routeunprotected", true) {
				found = true
			}
		}
		if !found {
			t.Errorf("falta el atributo routeunprotected=true: %v", spans[0].Attributes())
		}
	})

	t.Run("CF-03 ruta protegida: span sin routeunprotected", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		serve(t, []Authenticator{fake("a", Succeeded(nil))}, newReq("GET", "/graph/me", false), TraceProvider(tp))
		spans := sr.Ended()
		if len(spans) != 1 || len(spans[0].Attributes()) != 0 {
			t.Errorf("spans = %d, atributos = %v", len(spans), spans)
		}
	})
}
