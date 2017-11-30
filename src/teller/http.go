package teller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/gz-c/tollbooth"
	"github.com/rs/cors"
	"github.com/sirupsen/logrus"
	"github.com/unrolled/secure"
	"golang.org/x/crypto/acme/autocert"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/util/droplet"

	"github.com/skycoin/teller/src/addrs"
	"github.com/skycoin/teller/src/config"
	"github.com/skycoin/teller/src/exchange"
	"github.com/skycoin/teller/src/scanner"
	"github.com/skycoin/teller/src/util/httputil"
	"github.com/skycoin/teller/src/util/logger"
)

const (
	shutdownTimeout = time.Second * 5

	// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	// The timeout configuration is necessary for public servers, or else
	// connections will be used up
	serverReadTimeout  = time.Second * 10
	serverWriteTimeout = time.Second * 60
	serverIdleTimeout  = time.Second * 120

	// Directory where cached SSL certs from Let's Encrypt are stored
	tlsAutoCertCache = "cert-cache"
)

var (
	errInternalServerError = errors.New("Internal Server Error")
)

// HTTPServer exposes the API endpoints and static website
type HTTPServer struct {
	cfg           config.Config
	log           logrus.FieldLogger
	service       *Service
	httpListener  *http.Server
	httpsListener *http.Server
	quit          chan struct{}
	done          chan struct{}
}

// NewHTTPServer creates an HTTPServer
func NewHTTPServer(log logrus.FieldLogger, cfg config.Config, service *Service) *HTTPServer {
	return &HTTPServer{
		cfg: cfg.Redacted(),
		log: log.WithFields(logrus.Fields{
			"prefix": "teller.http",
		}),
		service: service,
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Run runs the HTTPServer
func (s *HTTPServer) Run() error {
	log := s.log
	log.WithField("config", s.cfg).Info("HTTP service start")
	defer log.Info("HTTP service closed")
	defer close(s.done)

	var mux http.Handler = s.setupMux()

	allowedHosts := []string{} // empty array means all hosts allowed
	sslHost := ""
	if s.cfg.Web.AutoTLSHost == "" {
		// Note: if AutoTLSHost is not set, but HTTPSAddr is set, then
		// http will redirect to the HTTPSAddr listening IP, which would be
		// either 127.0.0.1 or 0.0.0.0
		// When running behind a DNS name, make sure to set AutoTLSHost
		sslHost = s.cfg.Web.HTTPSAddr
	} else {
		sslHost = s.cfg.Web.AutoTLSHost
		// When using -auto-tls-host,
		// which implies automatic Let's Encrypt SSL cert generation in production,
		// restrict allowed hosts to that host.
		allowedHosts = []string{s.cfg.Web.AutoTLSHost}
	}

	if len(allowedHosts) == 0 {
		log = log.WithField("allowedHosts", "all")
	} else {
		log = log.WithField("allowedHosts", allowedHosts)
	}

	log = log.WithField("sslHost", sslHost)

	log.Info("Configured")

	secureMiddleware := configureSecureMiddleware(sslHost, allowedHosts)
	mux = secureMiddleware.Handler(mux)

	if s.cfg.Web.HTTPAddr != "" {
		s.httpListener = setupHTTPListener(s.cfg.Web.HTTPAddr, mux)
	}

	handleListenErr := func(f func() error) error {
		if err := f(); err != nil {
			select {
			case <-s.quit:
				return nil
			default:
				log.WithError(err).Error("ListenAndServe or ListenAndServeTLS error")
				return fmt.Errorf("http serve failed: %v", err)
			}
		}
		return nil
	}

	if s.cfg.Web.HTTPAddr != "" {
		log.Info(fmt.Sprintf("HTTP server listening on http://%s", s.cfg.Web.HTTPAddr))
	}
	if s.cfg.Web.HTTPSAddr != "" {
		log.Info(fmt.Sprintf("HTTPS server listening on https://%s", s.cfg.Web.HTTPSAddr))
	}

	var tlsCert, tlsKey string
	if s.cfg.Web.HTTPSAddr != "" {
		log.Info("Using TLS")

		s.httpsListener = setupHTTPListener(s.cfg.Web.HTTPSAddr, mux)

		tlsCert = s.cfg.Web.TLSCert
		tlsKey = s.cfg.Web.TLSKey

		if s.cfg.Web.AutoTLSHost != "" {
			log.Info("Using Let's Encrypt autocert")
			// https://godoc.org/golang.org/x/crypto/acme/autocert
			// https://stackoverflow.com/a/40494806
			certManager := autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(s.cfg.Web.AutoTLSHost),
				Cache:      autocert.DirCache(tlsAutoCertCache),
			}

			s.httpsListener.TLSConfig = &tls.Config{
				GetCertificate: certManager.GetCertificate,
			}

			// These will be autogenerated by the autocert middleware
			tlsCert = ""
			tlsKey = ""
		}

	}

	return handleListenErr(func() error {
		var wg sync.WaitGroup
		errC := make(chan error)

		if s.cfg.Web.HTTPAddr != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.httpListener.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.WithError(err).Println("ListenAndServe error")
					errC <- err
				}
			}()
		}

		if s.cfg.Web.HTTPSAddr != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.httpsListener.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
					log.WithError(err).Error("ListenAndServeTLS error")
					errC <- err
				}
			}()
		}

		done := make(chan struct{})

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case err := <-errC:
			return err
		case <-s.quit:
			return nil
		case <-done:
			return nil
		}
	})
}

func configureSecureMiddleware(sslHost string, allowedHosts []string) *secure.Secure {
	sslRedirect := true
	if sslHost == "" {
		sslRedirect = false
	}

	return secure.New(secure.Options{
		AllowedHosts: allowedHosts,
		SSLRedirect:  sslRedirect,
		SSLHost:      sslHost,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP
		// FIXME: Web frontend code has inline styles, CSP doesn't work yet
		// ContentSecurityPolicy: "default-src 'self'",

		// Set HSTS to one year, for this domain only, do not add to chrome preload list
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security
		STSSeconds:           31536000, // 1 year
		STSIncludeSubdomains: false,
		STSPreload:           false,

		// Deny use in iframes
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options
		FrameDeny: true,

		// Disable MIME sniffing in browsers
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Content-Type-Options
		ContentTypeNosniff: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-XSS-Protection
		BrowserXssFilter: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Referrer-Policy
		// "same-origin" is invalid in chrome
		ReferrerPolicy: "no-referrer",
	})
}

func setupHTTPListener(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
	}
}

func (s *HTTPServer) setupMux() *http.ServeMux {
	mux := http.NewServeMux()

	ratelimit := func(h http.Handler) http.Handler {
		limiter := tollbooth.NewLimiter(s.cfg.Web.ThrottleMax, s.cfg.Web.ThrottleDuration, nil)
		if s.cfg.Web.BehindProxy {
			limiter.SetIPLookups([]string{"X-Forwarded-For", "RemoteAddr", "X-Real-IP"})
		}
		return tollbooth.LimitHandler(limiter, h)
	}

	handleAPI := func(path string, h http.Handler) {
		// Allow requests from a local skycoin wallet
		h = cors.New(cors.Options{
			AllowedOrigins: []string{"http://127.0.0.1:6420"},
		}).Handler(h)

		h = gziphandler.GzipHandler(h)

		mux.Handle(path, h)
	}

	// API Methods
	handleAPI("/api/bind", ratelimit(httputil.LogHandler(s.log, BindHandler(s))))
	handleAPI("/api/status", ratelimit(httputil.LogHandler(s.log, StatusHandler(s))))
	handleAPI("/api/config", ConfigHandler(s))

	// Static files
	mux.Handle("/", gziphandler.GzipHandler(http.FileServer(http.Dir(s.cfg.Web.StaticDir))))

	return mux
}

// Shutdown stops the HTTPServer
func (s *HTTPServer) Shutdown() {
	s.log.Info("Shutting down HTTP server(s)")
	defer s.log.Info("Shutdown HTTP server(s)")
	close(s.quit)

	var wg sync.WaitGroup
	wg.Add(2)

	shutdown := func(proto string, ln *http.Server) {
		defer wg.Done()
		if ln == nil {
			return
		}
		log := s.log.WithFields(logrus.Fields{
			"proto":   proto,
			"timeout": shutdownTimeout,
		})

		defer log.Info("Shutdown server")
		log.Info("Shutting down server")

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := ln.Shutdown(ctx); err != nil {
			log.WithError(err).Error("HTTP server shutdown error")
		}
	}

	shutdown("HTTP", s.httpListener)
	shutdown("HTTPS", s.httpsListener)

	wg.Wait()

	<-s.done
}

// BindResponse http response for /api/bind
type BindResponse struct {
	DepositAddress string `json:"deposit_address,omitempty"`
	CoinType       string `json:"coin_type,omitempty"`
}

type bindRequest struct {
	SkyAddr  string `json:"skyaddr"`
	CoinType string `json:"coin_type"`
}

// BindHandler binds skycoin address with a bitcoin address
// Method: POST
// Accept: application/json
// URI: /api/bind
// Args:
//    {"skyaddr": "...", "coin_type": "BTC"}
func BindHandler(s *HTTPServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logger.FromContext(ctx)

		w.Header().Set("Accept", "application/json")

		if !validMethod(ctx, w, r, []string{http.MethodPost}) {
			return
		}

		if r.Header.Get("Content-Type") != "application/json" {
			errorResponse(ctx, w, http.StatusUnsupportedMediaType, errors.New("Invalid content type"))
			return
		}

		bindReq := &bindRequest{}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&bindReq); err != nil {
			err = fmt.Errorf("Invalid json request body: %v", err)
			errorResponse(ctx, w, http.StatusBadRequest, err)
			return
		}
		defer r.Body.Close()

		log = log.WithField("bindReq", bindReq)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		if bindReq.SkyAddr == "" {
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing skyaddr"))
			return
		}

		switch bindReq.CoinType {
		case scanner.CoinTypeBTC:
		case "":
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing coin_type"))
			return
		default:
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Invalid coin_type"))
			return
		}

		log.Info()

		if !verifySkycoinAddress(ctx, w, bindReq.SkyAddr) {
			return
		}

		if !s.cfg.Web.APIEnabled {
			errorResponse(ctx, w, http.StatusForbidden, errors.New("API disabled"))
			return
		}

		log.Info("Calling service.BindAddress")

		btcAddr, err := s.service.BindAddress(bindReq.SkyAddr)
		if err != nil {
			log.WithError(err).Error("service.BindAddress failed")
			if err != addrs.ErrDepositAddressEmpty && err != ErrMaxBoundAddresses {
				err = errInternalServerError
			}
			errorResponse(ctx, w, http.StatusInternalServerError, err)
			return
		}

		log = log.WithField("btcAddr", btcAddr)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info("Bound sky and btc addresses")

		if err := httputil.JSONResponse(w, BindResponse{
			DepositAddress: btcAddr,
			CoinType:       scanner.CoinTypeBTC,
		}); err != nil {
			log.WithError(err).Error(err)
		}
	}
}

// StatusResponse http response for /api/status
type StatusResponse struct {
	Statuses []exchange.DepositStatus `json:"statuses,omitempty"`
}

// StatusHandler returns the deposit status of specific skycoin address
// Method: GET
// URI: /api/status
// Args:
//     skyaddr
func StatusHandler(s *HTTPServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logger.FromContext(ctx)

		if !validMethod(ctx, w, r, []string{http.MethodGet}) {
			return
		}

		skyAddr := r.URL.Query().Get("skyaddr")
		if skyAddr == "" {
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing skyaddr"))
			return
		}

		log = log.WithField("skyAddr", skyAddr)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info()

		if !verifySkycoinAddress(ctx, w, skyAddr) {
			return
		}

		if !s.cfg.Web.APIEnabled {
			errorResponse(ctx, w, http.StatusForbidden, errors.New("API disabled"))
			return
		}

		log.Info("Sending StatusRequest to teller")

		depositStatuses, err := s.service.GetDepositStatuses(skyAddr)
		if err != nil {
			log.WithError(err).Error("service.GetDepositStatuses failed")
			errorResponse(ctx, w, http.StatusInternalServerError, errInternalServerError)
			return
		}

		log = log.WithFields(logrus.Fields{
			"depositStatuses":    depositStatuses,
			"depositStatusesLen": len(depositStatuses),
		})
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info("Got depositStatuses")

		if err := httputil.JSONResponse(w, StatusResponse{
			Statuses: depositStatuses,
		}); err != nil {
			log.WithError(err).Error(err)
		}
	}
}

// ConfigResponse http response for /api/config
type ConfigResponse struct {
	Enabled                  bool   `json:"enabled"`
	BtcConfirmationsRequired int64  `json:"btc_confirmations_required"`
	MaxBoundBtcAddresses     int    `json:"max_bound_btc_addrs"`
	SkyBtcExchangeRate       string `json:"sky_btc_exchange_rate"`
}

// ConfigHandler returns the teller configuration
// Method: GET
// URI: /api/config
func ConfigHandler(s *HTTPServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logger.FromContext(ctx)

		if !validMethod(ctx, w, r, []string{http.MethodGet}) {
			return
		}

		// Convert the exchange rate to a skycoin balance string
		rate := s.cfg.SkyExchanger.SkyBtcExchangeRate
		dropletsPerBTC, err := exchange.CalculateBtcSkyValue(exchange.SatoshisPerBTC, rate)
		if err != nil {
			log.WithError(err).Error("exchange.CalculateBtcSkyValue failed")
			errorResponse(ctx, w, http.StatusInternalServerError, errInternalServerError)
			return
		}

		skyPerBTC, err := droplet.ToString(dropletsPerBTC)
		if err != nil {
			log.WithError(err).Error("droplet.ToString failed")
			errorResponse(ctx, w, http.StatusInternalServerError, errInternalServerError)
			return
		}

		if err := httputil.JSONResponse(w, ConfigResponse{
			Enabled:                  s.cfg.Web.APIEnabled,
			BtcConfirmationsRequired: s.cfg.BtcScanner.ConfirmationsRequired,
			SkyBtcExchangeRate:       skyPerBTC,
			MaxBoundBtcAddresses:     s.cfg.Teller.MaxBoundBtcAddresses,
		}); err != nil {
			log.WithError(err).Error(err)
		}
	}
}

func validMethod(ctx context.Context, w http.ResponseWriter, r *http.Request, allowed []string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}

	w.Header().Set("Allow", strings.Join(allowed, ", "))

	status := http.StatusMethodNotAllowed
	errorResponse(ctx, w, status, errors.New("Invalid request method"))

	return false
}

func verifySkycoinAddress(ctx context.Context, w http.ResponseWriter, skyAddr string) bool {
	log := logger.FromContext(ctx)

	if _, err := cipher.DecodeBase58Address(skyAddr); err != nil {
		msg := fmt.Sprintf("Invalid skycoin address: %v", err)
		httputil.ErrResponse(w, http.StatusBadRequest, msg)
		log.WithFields(logrus.Fields{
			"status":  http.StatusBadRequest,
			"skyAddr": skyAddr,
		}).WithError(err).Info("Invalid skycoin address")
		return false
	}

	return true
}

func errorResponse(ctx context.Context, w http.ResponseWriter, code int, err error) {
	log := logger.FromContext(ctx)
	log.WithFields(logrus.Fields{
		"status":    code,
		"statusMsg": http.StatusText(code),
	}).WithError(err).Info()

	if err != errInternalServerError {
		httputil.ErrResponse(w, code, err.Error())
	} else {
		httputil.ErrResponse(w, code)
	}
}
