package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func getAspectRatioType(width, height float64) string {
	const tolerance = 0.01
	ratio :=  width/height

	// 16:9 ≈ 1.7778
	if math.Abs(ratio - 1.7778) < tolerance {
		return "16:9"
	}

	// 9:16 ≈ 0.5625
	if math.Abs(ratio - 0.5625) < tolerance {
		return "9:16"
	}

	return "other"
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	out.Bytes()

	var result map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return "", fmt.Errorf("failed to unmarshal ffprobe output: %v, output: %s", err, out.String())
	}

	// can get the width and height fields from result
	streams, ok := result["streams"].([]interface{})
	if !ok || len(streams) == 0 {
		return "", fmt.Errorf("no streams found in ffprobe output: %s", out.String())
	}

	stream := streams[0].(map[string]interface{})
	width := stream["width"].(float64)
	height := stream["height"].(float64)
	ratio := getAspectRatioType(width, height)

	return ratio, err
}

func processVideoForFastStart(filePath string) (string, error) {
	parts := strings.Split(filePath, ".")
	outputFilePath := strings.Join([]string{parts[0], "processing", parts[1]}, ".")

	fmt.Println("hello:", outputFilePath)

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := cmd.Run()

	return outputFilePath, err
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Set limit for the size of the video file
	var VIDEO_SIZE_LIMIT int64 = 1 << 30 // 1 GB
	http.MaxBytesReader(w, r.Body, VIDEO_SIZE_LIMIT)

	// Get the video ID from the request path
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user
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

	fmt.Println("uploading a video", videoID, "by user", userID)

	videoMeta, _ := cfg.db.GetVideo(videoID)
	if videoMeta.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload video", nil)
		return
	}

	r.ParseMultipartForm(VIDEO_SIZE_LIMIT)

	file, header, err := r.FormFile("video")
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

	// Check the content type of the uploaded file
	contentType := header.Header.Get("Content-Type")
	fileExt := strings.Split(contentType, "/")[1]
	mediaType, _, err := mime.ParseMediaType(contentType)

	if err != nil || (mediaType != "video/mp4") {
		respondWithError(w, http.StatusBadRequest, "Invalid media type. Only MP4 are allowed", err)
		return
	}

	// Generate a random file ID
	randomBytes := make([]byte, 32)
	_, _ = rand.Read(randomBytes)
	fileId := base64.RawURLEncoding.EncodeToString(randomBytes)
	filePath := fmt.Sprintf("%s.%s", fileId, fileExt)

	// Create a temp file in file system
	tempFile, err := os.CreateTemp("", filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	// Write the file data to the temporary file
	_, err = io.Copy(tempFile, bytes.NewReader(fileData))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write file", err)
		return
	}

	// Reset the file pointer to the beginning of the temp file
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to seek to beginning of temp file", err)
		return
	}

	// Get the aspect ratio of the video
	ratio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	var s3Prefix string
	if ratio == "16:9" {
		s3Prefix = "landscape"
	} else if ratio == "9:16" {
		s3Prefix = "portrait"
	} else {
		s3Prefix = "other"
	}

	processedVideoPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}

	fmt.Println("processedVideoPath:", processedVideoPath)

	processedVideoData, err := os.ReadFile(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to read processed video file: %v", err)
		return
	}

	partsPath := strings.Split(processedVideoPath, "/")
	processedS3FilePath := partsPath[len(partsPath) -1]
	s3FilePath := fmt.Sprintf("%s/%s", s3Prefix, processedS3FilePath)

	fmt.Println("Uploading video to S3 with path:", s3FilePath)

	// Upload the video to S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(s3FilePath),
		Body:        bytes.NewReader(processedVideoData),
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}

	// Update video metadata with video URL
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3FilePath)
	videoMeta.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(videoMeta)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMeta)
}
