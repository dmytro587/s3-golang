package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	fileData, err := io.ReadAll(file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read file data", err)
		return
	}

	contentType := header.Header.Get("Content-Type")
	fileExt := strings.Split(contentType, "/")[1]

	mediaType, _, err := mime.ParseMediaType(contentType)

	if err != nil || (mediaType != "image/jpeg" && mediaType != "image/png") {
		respondWithError(w, http.StatusBadRequest, "Invalid media type. Only JPEG and PNG are allowed", err)
		return
	}

	videoMeta, _ := cfg.db.GetVideo(videoID)
	if videoMeta.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload thumbnails for this video", nil)
		return
	}

	randomBytes := make([]byte, 32)
	_, _ = rand.Read(randomBytes)
	fileId := base64.RawURLEncoding.EncodeToString(randomBytes)
	filePath := filepath.Join(
		cfg.assetsRoot,
		fmt.Sprintf("/%s.%s", fileId, fileExt),
	)

	fmt.Println("Saving thumbnail to", filePath)

	// Create a file in file system
	newFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file", err)
		return
	}
	defer newFile.Close()

	_, err = io.Copy(newFile, bytes.NewReader(fileData))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write file", err)
		return
	}

	// Update video metadata with thumbnail URL
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s.%s", cfg.port, fileId, fileExt)
	videoMeta.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(videoMeta)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMeta)
}
