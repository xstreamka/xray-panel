package handlers

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"
	"sync"
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

// Render выполняет шаблон страницы — вызывает "base" как entry point
func (r *Renderer) Render(w io.Writer, name string, data any) error {
	r.mu.RLock()
	tmpl, ok := r.templates[name]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("template %s not found", name)
	}

	return tmpl.ExecuteTemplate(w, "base", data)
}
