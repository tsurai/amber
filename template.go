package jantar

import (
	"fmt"
	"github.com/howeyc/fsnotify"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// TemplateManager hooks
const (
	TmBeforeParse  = iota
	TmBeforeRender = iota
	tmLast         = iota
)

// TemplateManager is responsible for loading, watching and rendering templates
type TemplateManager struct {
	hooks
	directory string
	watcher   *fsnotify.Watcher
	tmplFuncs template.FuncMap
	tmplList  *template.Template
}

func newTemplateManager(directory string) *TemplateManager {
	funcs := template.FuncMap{
		"antiClickjacking": func() template.HTML {
			return template.HTML("<style id=\"antiClickjack\">body{display:none !important;}</style>")
		},
		"set": func(args map[string]interface{}, key string, value interface{}) string {
			if args != nil {
				args[key] = value
			}
			return ""
		},
		"array": func(args ...interface{}) []interface{} {
			var ret []interface{}
			for _, arg := range args {
				ret = append(ret, arg)
			}
			return ret
		},
		"toHtml": func(str string) template.HTML {
			return template.HTML(str)
		},
		"url": func(name string, args ...interface{}) string {
			router := GetModule(ModuleRouter).(*router)
			return router.getReverseURL(name, args)
		},
		"since": func(t time.Time) string {
			seconds := int(time.Since(t).Seconds())
			if seconds < 60 {
				return "< 1 minute ago"
			} else if seconds < 60*2 {
				return "1 minute ago"
			} else if seconds < 60*60 {
				return fmt.Sprintf("%d minutes ago", seconds/60)
			} else if seconds < 60*60*2 {
				return "1 hour ago"
			} else if seconds < 60*60*24 {
				return fmt.Sprintf("%d hours ago", seconds/(60*60))
			} else if seconds < 60*60*24*2 {
				return "1 day ago"
			} else if seconds < 60*60*24*30 {
				return fmt.Sprintf("%d days ago", seconds/(60*60*24))
			} else if seconds < 60*60*24*30*2 {
				return "1 month ago"
			} else if seconds < 60*60*24*30*12 {
				return fmt.Sprintf("%d months ago", seconds/(60*60*24*30))
			}
			return "> 1 year ago"
		},
		"paginate": func(curPage int, nPages int, offset int, url string) template.HTML {
			if nPages < 2 {
				return template.HTML("")
			}

			result := "<ul class='pagination'>"

			if curPage > 1 {
				result += fmt.Sprintf("<li><a href='%s/page/first'>&laquo;First</a></li>"+
					"<li><a href='%[1]s/page/%d'>&laquo;</a></li>", url, curPage-1)
			}

			if curPage-offset > 1 {
				result += "<li><span>...</span></li>"
			}

			for i := curPage - offset; i < curPage+offset+1; i++ {
				if i > 0 && i <= nPages {
					if i == curPage {
						result += fmt.Sprintf("<li class='active'><a href='%s/page/%d'>%[2]d</a></li>", url, i)
					} else {
						result += fmt.Sprintf("<li><a href='%s/page/%d'>%[2]d</a></li>", url, i)
					}
				}
			}

			if curPage+offset < nPages {
				result += "<li><span>...</span></li>"
			}

			if curPage != nPages {
				result += fmt.Sprintf("<li><a href='%s/page/%d'>&raquo;</a></li>"+
					"<li><a href='%[1]s/page/last'>Last&raquo;</a></li>", url, curPage+1)
			}
			return template.HTML(result + "</ul>")
		},
	}

	tm := &TemplateManager{directory: strings.Replace(strings.ToLower(directory), "\\", "/", -1), tmplFuncs: funcs}

	// register hooks
	tm.registerHook(TmBeforeParse, reflect.TypeOf(
		(func(*TemplateManager, string, *[]byte))(nil)))
	tm.registerHook(TmBeforeRender, reflect.TypeOf(
		(func(*http.Request, *TemplateManager, *template.Template, map[string]interface{}))(nil)))

	return tm
}

// watch listens for file events and reloads templates on changes
func (tm *TemplateManager) watch() {
	for {
		select {
		case ev := <-tm.watcher.Event:
			if !ev.IsRename() && filepath.Ext(ev.Name) == ".html" {
				Log.Debug("reloading templates")
				go tm.loadTemplates()
				return
			}
		case err := <-tm.watcher.Error:
			Log.Warningdf(JLData{"error": err}, "file watcher error")
			return
		}
	}
}

func (tm *TemplateManager) loadTemplates() error {
	var err error
	var templates *template.Template
	var staticTemplates *template.Template

	// close watcher if running
	if tm.watcher != nil {
		tm.watcher.Close()
	}

	// create a new watcher and start the watcher thread
	if tm.watcher, err = fsnotify.NewWatcher(); err != nil {
		return err
	}
	go tm.watch()

	// walk resursive through the template directory
	ret := filepath.Walk(tm.directory, func(path string, info os.FileInfo, err error) error {
		static := false
		path = strings.Replace(strings.ToLower(path), "\\", "/", -1)

		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") || info.Name() == "_static" {
				return filepath.SkipDir
			}

			// add the current directory to the watcher
			if err = tm.watcher.Watch(path); err != nil {
				Log.Warningdf(JLData{"error": err.Error()}, "can't watch directory '%s'", path)
			}
			return nil
		}

		if strings.HasSuffix(info.Name(), ".html") {
			if strings.HasPrefix(path, tm.directory+"/static/") {
				static = true
			}

			fdata, err := ioutil.ReadFile(path)
			if err != nil {
				Log.Error(err)
				return err
			}

			tmplName := path[len(tm.directory)+1:]

			// is it a static file?
			if static {
				if staticTemplates == nil {
					staticTemplates, err = template.New(tmplName).Parse(string(fdata))
				} else {
					staticTemplates, err = staticTemplates.New(tmplName).Parse(string(fdata))
				}

				if err == nil && !strings.HasPrefix(tmplName, "_") && !strings.Contains(tmplName, "/_") {
					filename := tm.directory + "/_" + tmplName
					if err = os.MkdirAll(filename[:len(filename)-len(info.Name())-1], os.ModePerm); err == nil {
						var f *os.File
						if f, err = os.Create(filename); err == nil {
							err = staticTemplates.Lookup(tmplName).Execute(f, nil)
						}
					}
				}
			} else {
				// call BEFORE_PARSE hooks
				hooks := tm.getHooks(TmBeforeParse)
				for _, hook := range hooks {
					hook.(func(*TemplateManager, string, *[]byte))(tm, tmplName, &fdata)
				}

				// add the custom template functions to the first template
				if templates == nil {
					templates, err = template.New(tmplName).Funcs(tm.tmplFuncs).Parse(string(fdata))
				} else {
					_, err = templates.New(tmplName).Parse(string(fdata))
				}
			}

			if err != nil {
				Log.Error(err)
				return err
			}
		}
		return nil
	})

	// no errors occured, override the old list
	if ret == nil {
		tm.tmplList = templates
	}

	return ret
}

func (tm *TemplateManager) getTemplate(name string) *template.Template {
	if tm.tmplList == nil {
		return nil
	}

	return tm.tmplList.Lookup(strings.ToLower(name))
}

// AddTmplFunc adds a template function with a given name and function pointer.
// Note: AddTmplFunc has no effect if called after the templates have been parsed.
func (tm *TemplateManager) AddTmplFunc(name string, fn interface{}) {
	tm.tmplFuncs[name] = fn
}

// RenderTemplate renders a template with the given name and arguments.
// Note: A Controller should call its Render function instead.
func (tm *TemplateManager) RenderTemplate(w io.Writer, req *http.Request, name string, args map[string]interface{}) error {
	tmpl := tm.getTemplate(name)
	if tmpl == nil {
		return fmt.Errorf("can't find template '%s'", strings.ToLower(name))
	}

	// call BEFORE_RENDER hooks
	hooks := tm.getHooks(TmBeforeRender)
	for _, hook := range hooks {
		hook.(func(*http.Request, *TemplateManager, *template.Template, map[string]interface{}))(req, tm, tmpl, args)
	}

	if err := tmpl.Execute(w, args); err != nil {
		return fmt.Errorf("failed to render template. Reason: %s", err.Error())
	}

	return nil
}
