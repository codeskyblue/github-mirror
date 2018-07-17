package main

import (
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DeanThompson/syncmap"
	"github.com/c2h5oh/datasize"
	"github.com/franela/goreq"
	"github.com/pkg/errors"
)

var (
	port    int
	dataDir string
)

func HashString(s string) string {
	m := md5.New()
	m.Write([]byte(s))
	return fmt.Sprintf("%x", m.Sum(nil))
}

type Status struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Copied   int    `json:"int"`
	Total    int    `json:"int"`
}

func (s *Status) Write(p []byte) (int, error) {
	n := len(p)
	s.Copied += n
	return n, nil
}

type DownloadCache struct {
	CacheDir  string
	GetProxy  func() string
	mu        sync.Mutex
	dashboard *syncmap.SyncMap
	workers   map[string]bool
	waiters   map[string][]chan error
	serverMux *http.ServeMux
}

func NewDownloadCache(cacheDir string) *DownloadCache {
	if _, err := os.Stat(cacheDir); err != nil {
		os.MkdirAll(cacheDir, 0755)
	}
	dc := &DownloadCache{
		CacheDir:  cacheDir,
		workers:   make(map[string]bool),
		waiters:   make(map[string][]chan error),
		dashboard: syncmap.New(),
	}
	dc.initServeMux()
	return dc
}

func (d *DownloadCache) initServeMux() {
	m := http.NewServeMux()

	mirrors := make([]MirrorRule, 0)
	mirrors = append(mirrors, MirrorRule{
		regexp.MustCompile(`^/`),
		"https://github.com/",
	})

	m.HandleFunc("/_dashboard", func(w http.ResponseWriter, r *http.Request) {
		output := "<html><body><h2>Dashboard</h2><ul>"
		for item := range d.dashboard.IterItems() {
			st := item.Value.(*Status)
			percent := 0.0
			if st.Total > 0 {
				percent = float64(st.Copied) * 100 / float64(st.Total)
			}
			output += "<li>" + st.URL + "&nbsp;&nbsp;" +
				fmt.Sprintf("%.1f%% - %s / %s", percent,
					datasize.ByteSize(st.Copied).HR(), datasize.ByteSize(st.Total).HR()) + "</li>"
		}
		output += "</ul></body></html>"
		io.WriteString(w, output)
	})

	m.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		url := req.URL.Path
		matches := regexp.MustCompile(`.*/([^/?]+)`).FindStringSubmatch(url)
		downloadName := "cached.file"
		if matches != nil {
			downloadName = matches[1]
		}
		urlPrefix := ""
		for _, mirror := range mirrors {
			if mirror.Pattern.MatchString(url) {
				urlPrefix = mirror.URLPrefix
			}
		}
		if urlPrefix == "" {
			io.WriteString(rw, "Github Mirror")
			return
		}
		mirrorURL := strings.TrimSuffix(urlPrefix, "/") + req.RequestURI
		log.Println("mirror url:", mirrorURL)
		err := d.DownloadAndWait(mirrorURL, downloadName)
		if err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		downcache.ServeFile(rw, req, mirrorURL)
	})
	d.serverMux = m
}

func (d *DownloadCache) unsafeAddWaiter(hash string) chan error {
	if _, exists := d.waiters[hash]; !exists {
		d.waiters[hash] = make([]chan error, 0)
	}
	ch := make(chan error, 1)
	d.waiters[hash] = append(d.waiters[hash], ch)
	return ch
}

func (d *DownloadCache) unsafeNotifyWaiters(hash string, err error) {
	for _, ch := range d.waiters[hash] {
		ch <- err
	}
	delete(d.waiters, hash)
}

func (d *DownloadCache) download(url string, filename string) (err error) {
	req := goreq.Request{
		Method:          "GET",
		Uri:             url,
		MaxRedirects:    10,
		RedirectHeaders: true,
	}
	if d.GetProxy != nil {
		proxy := d.GetProxy()
		if !strings.HasPrefix(proxy, "http://") {
			log.Printf("Invalid proxy %s, must startswith http://", strconv.Quote(proxy))
		} else {
			req.Proxy = proxy
		}
	}
	hash := HashString(url)

	res, err := req.Do()
	if err != nil {
		return err
	}
	defer res.Body.Close()
	log.Println(res.StatusCode)

	if res.StatusCode != 200 {
		return errors.New("remote: " + res.Status)
	}
	fileLength, err := strconv.Atoi(res.Header.Get("Content-Length"))
	if err != nil {
		log.Printf("WARNING: %s content-length unknown", url)
	}

	tmpFilename := filepath.Join(d.CacheDir, HashString(url)+".tmp")
	targetDir := d.downloadDir(url)

	defer func() {
		if err != nil {
			os.Remove(tmpFilename)
			os.RemoveAll(targetDir)
		}
	}()

	var f *os.File
	f, err = os.Create(tmpFilename)
	if err != nil {
		return errors.Wrap(err, "create file")
	}

	st := &Status{
		URL:      url,
		Filename: filename,
		Total:    fileLength,
	}

	d.dashboard.Set(hash, st)
	defer d.dashboard.Delete(hash)

	var size int64
	size, err = io.Copy(io.MultiWriter(st, f), res.Body)
	if err != nil {
		f.Close()
		os.Remove(tmpFilename)
		return err
	}
	if err = f.Close(); err != nil {
		return
	}

	if err = os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	targetPath := filepath.Join(targetDir, "cached.file")
	if err = os.Rename(tmpFilename, targetPath); err != nil {
		return err
	}
	// time, url, size, filename
	metaData, _ := json.Marshal(map[string]interface{}{
		"filename": filename,
		"size":     size,
		"url":      url,
		"time":     time.Now().Unix(), // seconds elapsed
	})
	err = ioutil.WriteFile(filepath.Join(targetDir, "meta.json"), metaData, 0644)
	return err
}

func (d *DownloadCache) downloadDir(url string) string {
	hash := HashString(url)
	return filepath.Join(d.CacheDir, hash[:2], hash[2:])
}

func (d *DownloadCache) IsCached(url string) bool {
	_, err := os.Stat(d.downloadDir(url))
	return err == nil
}

func (d *DownloadCache) DownloadAndWait(url string, filename string) error {
	if filename == "" {
		filename = "cached.file"
	}
	dir := d.downloadDir(url)
	d.mu.Lock()
	// check if file exists
	if _, err := os.Stat(dir + "/meta.json"); err == nil {
		d.mu.Unlock()
		return nil
	}

	hash := HashString(url)
	// check if downloading
	if d.workers[hash] {
		waitChan := d.unsafeAddWaiter(hash)
		d.mu.Unlock()
		log.Println("join wait", filename)
		return <-waitChan // wait until finished
	}
	// start downloading
	d.workers[hash] = true
	d.mu.Unlock()

	log.Println("download", filename)
	err := d.download(url, filename)

	d.mu.Lock()
	d.unsafeNotifyWaiters(hash, err)
	d.mu.Unlock()
	log.Println("finished", filename, err)
	return err
}

// ServeFile serve static file
func (d *DownloadCache) ServeFile(w http.ResponseWriter, req *http.Request, url string) {
	dir := d.downloadDir(url)
	metaPath := filepath.Join(dir, "meta.json")
	metaData, err := ioutil.ReadFile(metaPath)
	if err != nil {
		http.Error(w, "404 Not Found", 404)
		return
	}
	var info struct {
		Filename string `json:"filename"`
		Size     int    `json:"size"`
		URL      string `json:"url"`
		Time     int64  `json:"time"`
	}
	if err = json.Unmarshal(metaData, &info); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	mtime := time.Now()
	os.Chtimes(metaPath, mtime, mtime)

	f, err := os.Open(filepath.Join(dir, "cached.file"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	modtime := time.Unix(info.Time, 0)
	http.ServeContent(w, req, info.Filename, modtime, f)
}

type MirrorRule struct {
	Pattern   *regexp.Regexp
	URLPrefix string
}

// Clean remove file which not accessed to long
// Note: every request will update meta.json mtime
func (d *DownloadCache) Clean(keepDuration time.Duration) {
	filepath.Walk(d.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Printf("prevent panic by handling failure accessing a path %q: %v\n", d.CacheDir, err)
			return err
		}
		if info.Name() != "meta.json" {
			return nil
		}

		existsDuration := time.Since(info.ModTime())
		if existsDuration > keepDuration {
			log.Println("clean", path, existsDuration)
			os.RemoveAll(filepath.Dir(path))
		}
		return nil
	})
}

func (d *DownloadCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.serverMux.ServeHTTP(w, r)
}

var downcache *DownloadCache

func main() {
	var proxy string
	flag.IntVar(&port, "p", 8000, "Listen port")
	flag.StringVar(&proxy, "proxy", "", "Proxy addr or command to get proxy")
	flag.StringVar(&dataDir, "d", "data", "cached data store path")
	flag.Parse()

	downcache = NewDownloadCache(dataDir)
	go func() {
		for {
			downcache.Clean(time.Hour * 24 * 7)
			time.Sleep(1 * time.Hour)
		}
	}()

	if strings.HasPrefix(proxy, "http://") {
		downcache.GetProxy = func() string {
			return proxy
		}
	} else if proxy != "" {
		downcache.GetProxy = func() string {
			output, err := exec.Command("bash", "-c", proxy).Output()
			if err != nil {
				log.Printf("command: %s error %v", proxy, err)
				return ""
			} else {
				return strings.TrimSpace(string(output))
			}
		}
	}
	log.Printf("github-mirror listen on :%d", port)
	http.ListenAndServe(":"+strconv.Itoa(port), downcache)
}
