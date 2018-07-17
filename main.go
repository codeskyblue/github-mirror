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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/franela/goreq"
	"github.com/gorilla/mux"
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

type DownloadCache struct {
	CacheDir string
	GetProxy func() string
	mu       sync.Mutex
	workers  map[string]bool
	waiters  map[string][]chan error
}

func NewDownloadCache(cacheDir string) *DownloadCache {
	if _, err := os.Stat(cacheDir); err != nil {
		os.MkdirAll(cacheDir, 0755)
	}
	return &DownloadCache{
		CacheDir: cacheDir,
		workers:  make(map[string]bool),
		waiters:  make(map[string][]chan error),
	}
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

	res, err := req.Do()
	if err != nil {
		return err
	}
	defer res.Body.Close()
	log.Println(res.StatusCode)

	if res.StatusCode != 200 {
		return errors.New("remote: " + res.Status)
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
	var size int64
	size, err = io.Copy(f, res.Body)
	if err != nil {
		f.Close()
		os.Remove(tmpFilename)
		return err
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
	log.Println("finished", filename)
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
	f, err := os.Open(filepath.Join(dir, "cached.file"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	modtime := time.Unix(info.Time, 0)
	http.ServeContent(w, req, info.Filename, modtime, f)
}

var downcache *DownloadCache

func init() {
	r := mux.NewRouter()
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Github mirror, test with some github file under releases")
	})

	// http://localhost:8000/openatx/android-uiautomator-server/releases/download/1.1.4/app-uiautomator-test.apk
	// http://localhost:8000/openatx/atx-agent/releases/download/0.3.5/atx-agent_0.3.5_checksums.txt
	r.HandleFunc("/{repo}/{name}/releases/download/{version}/{filename}", func(w http.ResponseWriter, r *http.Request) {
		log.Println("request:", r.RequestURI)
		filename := mux.Vars(r)["filename"]
		origURL := "https://github.com" + r.RequestURI
		err := downcache.DownloadAndWait(origURL, filename)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		downcache.ServeFile(w, r, origURL)
	})

	http.Handle("/", r)
}

func main() {
	var proxy string
	flag.IntVar(&port, "p", 8000, "Listen port")
	flag.StringVar(&proxy, "proxy", "", "Proxy addr or command to get proxy")
	flag.StringVar(&dataDir, "d", "data", "cached data store path")
	flag.Parse()

	downcache = NewDownloadCache(dataDir)
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
	http.ListenAndServe(":"+strconv.Itoa(port), nil)
}
