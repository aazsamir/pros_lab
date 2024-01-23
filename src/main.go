package main

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"golang.org/x/exp/slog"
)

func main() {
	godotenv.Load()
	initLogger()
	slog.Info("Starting...")
	port := os.Getenv("APP_PORT")

	router := Router{}
	slog.Info("Listening on port " + port + "...")
	http.ListenAndServe(":"+port, &router)
}

func initLogger() {
	lvl := new(slog.LevelVar)
	envLevel, err := strconv.Atoi(os.Getenv("LOG_LEVEL"))
	if err != nil {
		panic(err)
	}
	lvl.Set(slog.Level(envLevel))
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
}

type Router struct{}

// /api/1920x1080/ftp.pl/filename.jpg
//
// /api/{WIDTH}x{HEIGHT}/{URL}/{FILENAME}
func (rtr *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("Request", "url", r.URL.String())
	fragments := strings.Split(r.URL.Path, "/")

	if len(fragments) < 4 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	if fragments[1] != "api" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	width, height, err := getDimensions(fragments[2])

	if err != nil {
		http.Error(w, "Invalid dimensions", http.StatusNotFound)
		return
	}

	path := strings.Join(fragments[3:], "/")

	Handle(w, r, width, height, path)
}

// get dimensions tuple (width, height) from string (widthxheight)
func getDimensions(path string) (int, int, error) {
	regexp := regexp.MustCompile(`^(\d+)x(\d+)$`)
	matches := regexp.FindStringSubmatch(path)

	if len(matches) != 3 {
		return 0, 0, fmt.Errorf("dimensions not found")
	}

	width, err := strconv.Atoi(matches[1])

	if err != nil {
		return 0, 0, fmt.Errorf("width is not a number")
	}

	height, err := strconv.Atoi(matches[2])

	if err != nil {
		return 0, 0, fmt.Errorf("height is not a number")
	}

	return width, height, nil
}

// handle endpoint
func Handle(w http.ResponseWriter, r *http.Request, width int, height int, path string) {
	slog.Debug("Handle", "width", width, "height", height, "path", path)

	// is it a proper URL?
	url, err := url.Parse(path)
	if err != nil {
		http.Error(w, "URL is not valid", http.StatusNotFound)
		return
	}

	// is it in allowed hosts list?
	if !isAllowedHost(*url) {
		http.Error(w, "Host is not allowed", http.StatusBadRequest)
		return
	}

	// does it have allowed extension
	hasAllowedExtension := false
	for _, ext := range allowedExtensions() {
		if strings.HasSuffix(url.Path, "."+ext) {
			hasAllowedExtension = true
			break
		}
	}
	if !hasAllowedExtension {
		http.Error(w, "Not allowed file extension", http.StatusNotFound)
		return
	}

	// is image downloadable?
	image, err := getImage(*url, width, height)
	if err != nil {
		http.Error(w, "Error getting image", http.StatusNotFound)
		return
	}

	// is saved image readable?
	imageData, err := image.Get()
	if err != nil {
		http.Error(w, "Error reading image", http.StatusNotFound)
		return
	}

	// write response
	_, err = w.Write(imageData)
	if err != nil {
		http.Error(w, "Error writing image", http.StatusNotFound)
		return
	}
}

func isAllowedHost(url url.URL) bool {
	hostsEnv := os.Getenv("APP_ALLOWED_HOSTS")
	hosts := strings.Split(hostsEnv, ",")

	urlhost := strings.Split(url.String(), "/")[0]

	for _, host := range hosts {
		if host == urlhost {
			slog.Info("isAllowedHost", "host", host, "url", url.Host)
			return true
		}
	}

	return false
}

func allowedExtensions() [3]string {
	return [3]string{"jpg", "jpeg", "png"}
}

type Image struct {
	Width    int
	Height   int
	Filename string
}

func (img *Image) Path() string {
	return "./var/" + img.Filename
}

func (img *Image) FinalPath() string {
	return fmt.Sprintf("./var/%sx%s/"+img.Filename, strconv.Itoa(img.Width), strconv.Itoa(img.Height))
}

func (img *Image) Extension() string {
	return "png"
}

// Get image from filesystem
func (img *Image) Get() ([]byte, error) {
	file, err := os.Open(img.Filename)

	if err != nil {
		slog.Error("Get::open", "error", err)
		return nil, err
	}

	defer file.Close()

	image, err := io.ReadAll(file)

	if err != nil {
		slog.Error("Get::read", "error", err)
		return nil, err
	}

	return image, nil
}

// Get from filesystem, or download and upscale
func getImage(url url.URL, width int, height int) (*Image, error) {
	hash := pathFriendlyHash(url.String())
	path := fmt.Sprintf("./var/%dx%d/%s", width, height, hash)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		slog.Debug("downloading")
		return downloadAndUpscaleImage(url, width, height)
	}

	slog.Debug("cached")

	image := Image{
		Filename: path,
		Width:    width,
		Height:   height,
	}

	slog.Debug("Handle", "image", image.FinalPath())

	return &image, nil
}

// Download and upscale
func downloadAndUpscaleImage(url url.URL, width int, height int) (*Image, error) {
	image, err := downloadImage(url, width, height)

	if err != nil {
		return nil, err
	}

	image, err = upscaleImage(image)

	if err != nil {
		return nil, err
	}

	return image, nil
}

// Download
func downloadImage(url url.URL, width int, height int) (*Image, error) {
	slog.Debug("downloadImage", "url", url)

	response, err := http.Get("https://" + url.String())

	if err != nil {
		slog.Error("downloadImage::download", "error", err)
		return nil, err
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		slog.Error("downloadImage::statuscode", "error", "response status code", response.StatusCode)
		return nil, err
	}

	filename := pathFriendlyHash(url.String())
	file, err := os.Create("./var/" + filename)

	if err != nil {
		slog.Error("downloadImage::createfile", "error", err)
		return nil, err
	}

	defer file.Close()

	_, err = io.Copy(file, response.Body)

	if err != nil {
		slog.Error("downloadImage::saveImage", "error", err)
		return nil, err
	}

	image := Image{
		Filename: filename,
		Width:    width,
		Height:   height,
	}

	return &image, nil
}

// Make hash from URL, that can be used as filename
func pathFriendlyHash(s string) string {
	hasher := md5.New()
	hasher.Write([]byte(s))
	hashSum := hasher.Sum(nil)
	base64Hash := base64.URLEncoding.EncodeToString(hashSum)
	filePathFriendlyHash := strings.TrimRight(base64Hash, "=")
	extension := strings.Split(s, ".")[len(strings.Split(s, "."))-1]
	filePathFriendlyHash = filePathFriendlyHash + "." + extension

	return filePathFriendlyHash
}

// Upscale using RealESRGAN and resize with imagemagick
func upscaleImage(image *Image) (*Image, error) {
	command := fmt.Sprintf("./lib/realesr/realesrgan-ncnn-vulkan -i %s -o %s -n realesrgan-x4plus -f jpg -s 4", image.Path(), image.FinalPath())
	slog.Debug("upscaleImage", "command", command)

	out, err := exec.Command(
		"/bin/sh",
		"-c",
		command,
	).Output()

	if err != nil {
		slog.Error("upscaleImage", "error", err, "out", string(out))
		return nil, err
	}

	image = &Image{
		Filename: image.FinalPath(),
		Width:    image.Width,
		Height:   image.Height,
	}

	image, err = resizeImage(*image)

	if err != nil {
		slog.Error("upscaleImage::resize", "error", err)
		return nil, err
	}

	return image, nil
}

// Resize with imagemagick
func resizeImage(image Image) (*Image, error) {
	command := fmt.Sprintf("convert %s -resize %sx%s %s", image.Filename, strconv.Itoa(image.Width), strconv.Itoa(image.Height), image.Filename)
	slog.Debug("resizeImage", "command", command)

	_, err := exec.Command(
		"/bin/sh",
		"-c",
		command,
	).Output()

	if err != nil {
		slog.Error("resizeImage", "error", err)
		return nil, err
	}

	return &Image{
		Filename: image.Filename,
		Width:    image.Width,
		Height:   image.Height,
	}, nil
}
