package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

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

	vid, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if vid.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Video doesn't belong to user", nil)
		return
	}

	videoFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error reading video file", err)
		return
	}
	defer videoFile.Close()

	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for thumbnail", nil)
		return
	}

	mimeType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type for thumbnail", err)
		return
	}

	if mimeType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid video format", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	_, err = io.Copy(tempFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying video file", err)
		return
	}
	err = tempFile.Sync()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error syncing temp file", err)
		return
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error seeking processed video", err)
		return
	}

	bucketKey, err := MakeBucketKey(strings.Split(mimeType, "/")[1])
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating bucket key", err)
		return
	}
	aspect, err := getVideoAspectRatio(tempFile.Name())
	if err == nil {
		bucketKey = fmt.Sprintf("%s/%s", aspect, bucketKey)
	}

	processedVidPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedVidPath)
	processedVid, err := os.Open(processedVidPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video", err)
		return
	}
	defer processedVid.Close()

	_, err = processedVid.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error seeking processed video", err)
		return
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &bucketKey,
		Body:        processedVid,
		ContentType: &mimeType,
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading video", err)
		return
	}

	newVideoURL := fmt.Sprintf("https://%s.cloudfront.net/%s", cfg.s3CfDistribution, bucketKey)
	vid.VideoURL = &newVideoURL

	cfg.db.UpdateVideo(vid)
	respondWithJSON(w, http.StatusOK, vid)
}

func MakeBucketKey(extension string) (string, error) {
	token := make([]byte, 32)
	_, err := rand.Read(token)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(token) + "." + extension, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	type VideoStream struct {
		CodecType string `json:"codec_type"`
		Aspect    string `json:"display_aspect_ratio"`
	}
	type ProbeResult struct {
		Streams []VideoStream `json:"streams"`
	}
	var vids ProbeResult
	if err = json.Unmarshal(out.Bytes(), &vids); err != nil {
		return "", err
	}
	for _, vid := range vids.Streams {
		if vid.CodecType == "video" {
			if vid.Aspect == "16:9" {
				return "landscape", nil
			}
			if vid.Aspect == "9:16" {
				return "portrait", nil
			}
		}
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", filePath+".processing")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %s, %v", stderr.String(), err)
	}
	return filePath + ".processing", nil
}
