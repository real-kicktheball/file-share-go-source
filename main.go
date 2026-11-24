package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxFileSize = 10 << 20
const maxPixels = 16_000_000

//go:embed index.html
var indexPage []byte

var shareName = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg)$`)
var formats = map[string]string{".png": "png", ".jpg": "jpeg", ".jpeg": "jpeg"}

type service struct {
	root    *os.Root
	baseURL string
	token   string
	uploads chan struct{}
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	base := os.Getenv("PUBLIC_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		log.Fatal("PUBLIC_BASE_URL에는 http 또는 https로 시작하는 서버 주소를 입력해주세요")
	}
	token := os.Getenv("UPLOAD_TOKEN")
	if token == "" {
		log.Fatal("UPLOAD_TOKEN을 설정해주세요")
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer root.Close()
	s := &service{root: root, baseURL: strings.TrimRight(base, "/"), token: token, uploads: make(chan struct{}, 2)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexPage)
	})
	mux.HandleFunc("POST /api/files", s.upload)
	mux.HandleFunc("GET /f/{name}", s.download)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: time.Minute}
	log.Printf("수신 주소 %s, 공유 주소 %s", addr, s.baseURL)
	log.Fatal(server.ListenAndServe())
}

func (s *service) upload(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "업로드 토큰을 확인해주세요", http.StatusUnauthorized)
		return
	}
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		http.Error(w, "다른 업로드 처리 중입니다. 잠시 후 다시 시도해주세요", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "요청이 너무 큽니다", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "file 필드가 필요합니다", http.StatusBadRequest)
		}
		return
	}
	defer file.Close()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		http.Error(w, "파일은 한 개씩 올려주세요", http.StatusBadRequest)
		return
	}
	if header.Size > maxFileSize {
		http.Error(w, "파일은 10 MiB까지 올릴 수 있습니다", http.StatusRequestEntityTooLarge)
		return
	}
	want := formats[strings.ToLower(filepath.Ext(header.Filename))]
	if want == "" {
		http.Error(w, "PNG/JPEG만 올릴 수 있습니다", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "파일 읽기 실패", http.StatusBadRequest)
		return
	}
	if http.DetectContentType(data) != "image/"+want {
		http.Error(w, "확장자와 MIME 타입이 다릅니다", http.StatusUnsupportedMediaType)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != want || cfg.Width < 1 || cfg.Height < 1 {
		http.Error(w, "이미지 헤더를 읽을 수 없습니다", http.StatusUnsupportedMediaType)
		return
	}
	if cfg.Width > maxPixels/cfg.Height {
		http.Error(w, "이미지는 1600만 픽셀까지 올릴 수 있습니다", http.StatusRequestEntityTooLarge)
		return
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		http.Error(w, "이미지를 끝까지 읽을 수 없습니다", http.StatusUnsupportedMediaType)
		return
	}
	ext := ".png"
	if want == "jpeg" {
		ext = ".jpg"
	}
	name, err := s.save(data, ext)
	if err != nil {
		log.Printf("저장 실패: %v", err)
		http.Error(w, "저장 실패", http.StatusInternalServerError)
		return
	}
	link := s.baseURL + "/f/" + name
	w.Header().Set("Location", link)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": strings.TrimSuffix(name, ext), "url": link})
}

func (s *service) save(data []byte, ext string) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	name := hex.EncodeToString(id[:]) + ext
	tempName := ".upload-" + name
	tmp, err := s.root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer s.root.Remove(tempName)
	_, err = tmp.Write(data)
	closeErr := tmp.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	// 저장이 끝난 파일만 공개한다. 같은 이름이 이미 있으면 Link가 실패한다.
	return name, s.root.Link(tempName, name)
}

func (s *service) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !shareName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := s.root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/"+formats[filepath.Ext(name)])
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, info.ModTime(), f)
}
