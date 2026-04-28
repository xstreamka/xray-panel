package handlers

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sync"

	"xray-panel/internal/middleware"
)

type Renderer struct {
	templates map[string]*template.Template
	mu        sync.RWMutex
	funcMap   template.FuncMap
	layoutDir string
	pagesDir  string
}

func NewRenderer(layoutDir, pagesDir string, funcMap template.FuncMap) (*Renderer, error) {
	r := &Renderer{
		templates: make(map[string]*template.Template),
		funcMap:   funcMap,
		layoutDir: layoutDir,
		pagesDir:  pagesDir,
	}

	if err := r.loadAll(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Renderer) loadAll() error {
	layouts, err := filepath.Glob(filepath.Join(r.layoutDir, "*.html"))
	if err != nil {
		return fmt.Errorf("glob layouts: %w", err)
	}

	pages, err := filepath.Glob(filepath.Join(r.pagesDir, "*", "*.html"))
	if err != nil {
		return fmt.Errorf("glob pages: %w", err)
	}

	for _, page := range pages {
		name := filepath.Base(page)

		files := make([]string, 0, len(layouts)+1)
		files = append(files, layouts...)
		files = append(files, page)

		tmpl, err := template.New("").Funcs(r.funcMap).ParseFiles(files...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}

		r.templates[name] = tmpl
	}

	return nil
}

// Render выполняет шаблон, пишет в буфер, при ошибке отдаёт 500 и логирует.
// Принимает *http.Request, чтобы достать CSRF-токен из контекста и положить его
// в data под ключом CSRFToken — шаблоны вставляют его в формы через
// {{template "csrf" .}}.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, name string, data map[string]any) error {
	r.mu.RLock()
	tmpl, ok := r.templates[name]
	r.mu.RUnlock()

	if !ok {
		err := fmt.Errorf("template %s not found", name)
		log.Printf("Renderer: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return err
	}

	if data == nil {
		data = map[string]any{}
	}
	if req != nil {
		if t := middleware.CSRFTokenFromContext(req.Context()); t != "" {
			data["CSRFToken"] = t
		}
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		log.Printf("Renderer: execute %s failed: %v", name, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return err
	}

	_, err := buf.WriteTo(w)
	return err
}
