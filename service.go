package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	_ "embed"
	"encoding/base32"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/exp/slog"

	"github.com/julienschmidt/httprouter"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Service struct {
	t       *template.Template
	cfg     Config
	builder *Builder
	dataDir string
}

type Config struct {
	host string
	dev  bool
	port int
}

type MP map[string]interface{}

func NewService(cfg Config, dataDir string) (*Service, error) {
	var t *template.Template
	t, err := template.New("").Funcs(template.FuncMap{
		"partial": func(name string, data interface{}) (template.HTML, error) {
			var buf bytes.Buffer
			err := t.ExecuteTemplate(&buf, name, data)
			return template.HTML(buf.String()), err
		},
	}).ParseFS(templatesFS, "templates/*")
	if err != nil {
		return nil, fmt.Errorf("error parsing templates: %w", err)
	}

	return &Service{
		t:       t,
		cfg:     cfg,
		builder: NewBuilder(),
		dataDir: dataDir,
	}, nil
}

var upgrader = websocket.Upgrader{}

func (s *Service) errorResp(w http.ResponseWriter, code int, err error) {
	w.Header().Add("Content-Type", "text/html")
	w.WriteHeader(code)
	if err == nil {
		err = fmt.Errorf(http.StatusText(code))
	}
	slog.Error("errorResp", "err", err)
	if err := s.t.ExecuteTemplate(w, "error.html", MP{"error": err, "dev": s.cfg.dev}); err != nil {
		slog.Error("Error rendering template", "err", err, "name", "error.html")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
func (s *Service) executeTemplate(w http.ResponseWriter, name string, data MP) {
	if data == nil {
		data = MP{}
	}
	data["inner_template"] = name
	s.executePartial(w, "application.html", data)
}

func (s *Service) executePartial(w http.ResponseWriter, name string, data MP) {
	if data == nil {
		data = MP{}
	}
	data["dev"] = s.cfg.dev
	if err := s.t.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("Error rendering template", err, "name", name)
		s.errorResp(w, http.StatusInternalServerError, err)
	}
}

func (s *Service) newHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	s.executeTemplate(w, "new.html", MP{
		"source_filename": "main.go",
		"server_source":   string(counterSrc),
		"html_source":     string(counterHTML),
	})
}

func (s *Service) buildStatus(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	s.partialErrorHandler(w, "_build_status.html", func() (data MP, err error) {
		buildID, _ := strconv.Atoi(r.URL.Query().Get("id"))
		build := s.builder.Get(buildID)
		if build == nil {
			return nil, fmt.Errorf("Build not found")
		}
		data = MP{"build": build.templateData()}
		if data["build"].(MP)["completed"].(bool) == false {
			// Build is ongoing, send status
			return data, nil
		}
		if *data["build"].(MP)["exit_code"].(*int) != 0 {
			// Build failed
			_ = os.RemoveAll(build.dir)
			return data, nil
		}

		build.Lock()
		defer build.Unlock()

		// TODO: also try and run the wasm here, return any errors. We should
		// not deploy things that will fail to run.
		slog.Info("Build complete, creating result", "id", build.ID)
		if _, err := os.Stat(build.dir); os.IsNotExist(err) {
			return nil, fmt.Errorf("build directory is missing")
		}
		s.builder.Delete(build.ID)
		wasmFile, err := os.Open(filepath.Join(build.dir, "main.wasm"))
		if err != nil {
			return nil, fmt.Errorf("error opening build result: %w", err)
		}
		htmlFile, err := os.Open(filepath.Join(build.dir, "index.html"))
		if err != nil {
			return nil, fmt.Errorf("error opening build result: %w", err)
		}
		slog.Info("Computing hash", "id", build.ID)
		hash, err := hashResult(wasmFile, htmlFile)
		if err != nil {
			return nil, fmt.Errorf("error hashing build result: %w", err)
		}
		slog.Info("Hash calculated", "id", build.ID, "hash", hash)
		data["result_name"] = hash
		_ = wasmFile.Close()
		_ = htmlFile.Close()
		appDir := filepath.Join(s.dataDir, hash)
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			// Cleanup if this already exists.
			_ = os.RemoveAll(build.dir)
		}
		if err := os.Rename(build.dir, appDir); err != nil {
			return nil, fmt.Errorf("error creating app dir: %w", err)
		}
		return data, nil
	})
}

type errorHandlerCB func() (data MP, err error)

func (s *Service) _tErrorHandler(handler func(w http.ResponseWriter, name string, data MP),
	w http.ResponseWriter,
	name string,
	cb errorHandlerCB,
) {
	data, err := cb()
	if data == nil {
		data = MP{}
	}
	if err != nil {
		data["error"] = err
	}
	handler(w, name, data)
}
func (s *Service) partialErrorHandler(w http.ResponseWriter, name string, cb errorHandlerCB) {
	s._tErrorHandler(s.executePartial, w, name, cb)
}
func (s *Service) templateErrorHandler(w http.ResponseWriter, name string, cb errorHandlerCB) {
	s._tErrorHandler(s.executeTemplate, w, name, cb)
}

func (s *Service) createHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	s.partialErrorHandler(w, "_build_status.html", func() (data MP, err error) {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		data = MP{}
		buildDir, err := os.MkdirTemp("", "")
		if err != nil {
			return data, err
		}

		if err := os.WriteFile(
			filepath.Join(buildDir, r.FormValue("source_filename")),
			[]byte(r.FormValue("server_source")),
			0666,
		); err != nil {
			return data, err
		}
		if err := os.WriteFile(
			filepath.Join(buildDir, "index.html"),
			[]byte(r.FormValue("html_source")),
			0666); err != nil {
			return data, err
		}
		build := s.builder.SubmitBuild(buildDir, "tinygo build -x -o main.wasm -target wasi main.go")
		data["build"] = build.templateData()
		return data, nil
	})
}
func (s *Service) wsHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	subdomain, found := strings.CutSuffix(r.Host, s.cfg.host)
	if !found || subdomain == "" {
		s.errorResp(w, http.StatusNotFound, fmt.Errorf("not found"))
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.errorResp(w, http.StatusBadRequest, err)
		return
	}
	for {
		_, msg, err := conn.ReadMessage()
		fmt.Println(msg, err)
		if err != nil {
			s.errorResp(w, http.StatusBadRequest, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("1"))
	}
}

func (s *Service) appInfoHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	s.templateErrorHandler(w, "info.html", func() (data MP, err error) {
		name := p.ByName("name")
		appDir := filepath.Join(s.dataDir, name)
		if _, err := os.Stat(appDir); os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return nil, fmt.Errorf("Application %q not found", name)
		}

		serverSrc, err := os.ReadFile(filepath.Join(appDir, "main.go"))
		if err != nil {
			return nil, fmt.Errorf("error reading main.go: %w", err)
		}
		htmlSrc, err := os.ReadFile(filepath.Join(appDir, "index.html"))
		if err != nil {
			return nil, fmt.Errorf("error reading index.html: %w", err)
		}
		return MP{
			"name":          name,
			"server_source": string(serverSrc),
			"html_source":   string(htmlSrc),
		}, nil
	})
}

func (s *Service) indexHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	entries, err := ioutil.ReadDir(s.dataDir)
	if err != nil {
		s.errorResp(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModTime().After(entries[j].ModTime()) })
	apps := []MP{}
	for _, e := range entries {
		apps = append(apps, MP{
			"name": e.Name(),
			"date": e.ModTime().Format(time.RFC3339),
		})
	}
	if len(apps) > 50 {
		apps = apps[:50]
	}
	s.executeTemplate(w, "index.html", MP{"apps": apps})
}

func (s *Service) appHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	name := p.ByName("name")
	s.executeTemplate(w, "app.html", MP{
		"name":   name,
		"iframe": "//" + name + "." + s.cfg.host,
	})
}

func (s *Service) methodNotAllowedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.errorResp(w, http.StatusMethodNotAllowed, fmt.Errorf("Method not allowed"))
	})
}

func (s *Service) appSourceHandler(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	name, _ := strings.CutSuffix(r.Host, "."+s.cfg.host)
	appDir := filepath.Join(s.dataDir, name)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		http.Error(w, fmt.Errorf("Application %q not found", name).Error(), http.StatusNotFound)
		return
	}

	htmlSrc, err := os.ReadFile(filepath.Join(appDir, "index.html"))
	if err != nil {
		http.Error(w, fmt.Errorf("error reading index.html: %w", err).Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(htmlSrc)
}

func (s *Service) Handler() http.Handler {
	// Main site router.
	mainRouter := httprouter.New()
	// Separate router to get around overlapping wildcard rules.
	namedPathRouter := httprouter.New()
	mainRouter.MethodNotAllowed = namedPathRouter
	mainRouter.NotFound = namedPathRouter
	namedPathRouter.MethodNotAllowed = s.methodNotAllowedHandler()
	// Router for apps serving on their own domains
	appSubdomainRouter := httprouter.New()
	appSubdomainRouter.MethodNotAllowed = s.methodNotAllowedHandler()

	namedPathRouter.GET("/new", s.newHandler)
	namedPathRouter.GET("/health", func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		fmt.Fprint(w, "I love you. I'm glad I exist.")
	})

	namedPathRouter.GET("/new/build-status", s.buildStatus)
	namedPathRouter.POST("/new", s.createHandler)

	appSubdomainRouter.GET("/", s.appSourceHandler)
	appSubdomainRouter.GET("/ws", s.wsHandler)

	mainRouter.GET("/", s.indexHandler)
	mainRouter.GET("/:name", func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		// httprouter doesn't support overlapping parameter names and named
		// routes so first check if this is a route name we have reserved.
		handle, params, _ := namedPathRouter.Lookup(http.MethodGet, r.URL.Path)
		if handle != nil {
			handle(w, r, params)
			return
		}
		s.appHandler(w, r, p)
	})
	mainRouter.GET("/:name/info", s.appInfoHandler)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if we're serving from a subdomain like app_name.256kb.
		subdomain, found := strings.CutSuffix(r.Host, s.cfg.host)
		if !found {
			s.errorResp(w, http.StatusNotFound, fmt.Errorf("not found"))
			return
		}
		// If we aren't, or if it's a known subdomain, use the main router.
		if subdomain == "www" || subdomain == "" {
			mainRouter.ServeHTTP(w, r)
		} else {
			appSubdomainRouter.ServeHTTP(w, r)
		}
	})

}

func makeAppDir() (dir string, err error) {
	user, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("error getting current user: %w", err)
	}
	appDir := filepath.Join(user.HomeDir, ".256kb")
	if fi, err := os.Stat(appDir); os.IsNotExist(err) {
		if err := os.Mkdir(appDir, 0777); err != nil {
			return "", fmt.Errorf("error making directory %q: %w", appDir, err)
		}
	} else if !fi.IsDir() {
		return "", fmt.Errorf("data directory location %q already exists but it is not a directory", err)
	}
	return appDir, nil
}

func main() {
	cfg := Config{
		host: "localhost:3001",
	}
	flag.StringVar(&cfg.host, "host", "localhost:3001", "The HTTP Host the application will accept requests from.")
	flag.BoolVar(&cfg.dev, "dev", false, "Run the server in development mode")
	flag.IntVar(&cfg.port, "port", 3000, "Listen to me...")
	flag.Parse()

	appDir, err := makeAppDir()
	if err != nil {
		log.Panicln(err)
	}

	service, err := NewService(cfg, appDir)
	if err != nil {
		log.Panicln(err)
	}
	_ = service

	slog.Info("Listening", "port", cfg.port)
	panic(http.ListenAndServe(":"+fmt.Sprint(cfg.port), logMiddleware(service.Handler())))

	i, err := NewInstance(context.Background(), "", "", counterWasm)
	if err != nil {
		log.Panicln(err)
	}
	if err := i.Listen(context.Background(), ":8080"); err != nil {
		log.Panicln(err)
	}
}

func hashResult(wasm, html io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, wasm); err != nil {
		return "", err
	}
	if _, err := io.Copy(h, html); err != nil {
		return "", err
	}
	return bytesToBase32Hash(h.Sum(nil)), nil
}

// BytesToBase32Hash copies nix here
// https://nixos.org/nixos/nix-pills/nix-store-paths.html
// The comments tell us to compute the base32 representation of the
// first 160 bits (truncation) of a sha256 of the above string:
func bytesToBase32Hash(b []byte) string {
	var buf bytes.Buffer
	_, _ = base32.NewEncoder(base32.StdEncoding, &buf).Write(b[:20])
	return strings.ToLower(buf.String())
}
