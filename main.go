package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	// "log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
)

// --- DATA STRUCTURES ---
type SceneData struct {
	Name    string `json:"name"`
	Details string `json:"details"`
}

type ScriptResponse struct {
	Intro string   `json:"intro"`
	Items []string `json:"items"`
	Outro string   `json:"outro"`
}

type TMDBSearchResponse struct {
	Results []struct {
		PosterPath string `json:"poster_path"`
	} `json:"results"`
}

func main() {
	// 1. Load Environment Variables (Ignore error in production)
	_ = godotenv.Load()

	r := gin.Default()

	// Serve the output folder so the app can play videos
	r.Static("/videos", "./output")

	// Increase upload limit for video files (100MB)
	r.MaxMultipartMemory = 100 << 20

	r.POST("/generate-multi-scene", func(c *gin.Context) {
		fmt.Println("\n🔹 STEP 1: Request Received") // DEBUG

		// 2. Parse Form Data
		topic := c.PostForm("topic")
		category := c.PostForm("category")
		videoType := c.PostForm("type")
		scenesJson := c.PostForm("scenes")

		if videoType == "" { videoType = "short" }

		var scenes []SceneData
		if err := json.Unmarshal([]byte(scenesJson), &scenes); err != nil {
			fmt.Println("❌ Error: Invalid JSON") // DEBUG
			c.JSON(400, gin.H{"error": "Invalid scenes JSON"})
			return
		}

		fmt.Printf("🎬 Topic: %s | Mode: %s | Items: %d\n", topic, videoType, len(scenes))

		// --- 3. MEDIA PROCESSING ENGINE ---
		saveMedia := func(formKey, fallbackName string, tryTMDB bool) string {
			file, err := c.FormFile(formKey)

			// A. User Uploaded a File
			if err == nil {
				ext := filepath.Ext(file.Filename)
				if ext == "" { ext = ".jpg" }
				savePath := fmt.Sprintf("output/%s%s", formKey, ext)
				c.SaveUploadedFile(file, savePath)
				fmt.Printf("📂 Saved Upload: %s\n", savePath)
				return savePath
			}

			// B. No Upload -> Use Fallback
			savePath := fmt.Sprintf("output/%s.jpg", formKey)

			// Try TMDB if it's a scene
			if tryTMDB && category == "movie" && fallbackName != "" {
				if err := downloadTMDBPoster(fallbackName, savePath); err == nil {
					return savePath
				}
			}

			// Placeholder
			txt := fallbackName
			if txt == "" { txt = "Scene" }
			downloadPlaceholder(txt, savePath, videoType)
			return savePath
		}

		// Save Media
		introPath := saveMedia("media_intro", topic, false)
		outroPath := saveMedia("media_outro", "Thanks for watching!", false)

		scenePaths := make([]string, len(scenes))
		for i := range scenes {
			scenePaths[i] = saveMedia(fmt.Sprintf("media_%d", i), scenes[i].Name, true)
		}

		// --- 4. AI SCRIPT GENERATION ---
		fmt.Println("🔹 STEP 2: Generating Script (Groq)...") // DEBUG
		scriptData, err := generateSegmentedScript(topic, category, videoType, scenes)
		if err != nil {
			fmt.Printf("❌ CRITICAL ERROR (Groq): %v\n", err) // DEBUG
			c.JSON(500, gin.H{"error": "AI Script failed: " + err.Error()})
			return
		}

		// --- 5. RENDER SEGMENTS (Google TTS + Low Memory FFmpeg) ---
		fmt.Println("🔹 STEP 3: Rendering Segments...") // DEBUG
		var segmentFiles []string

		// Render Intro
		introVid := "output/seg_intro.mp4"
		if err := renderSegment(scriptData.Intro, introPath, introVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, introVid)
		} else {
			fmt.Printf("❌ Error Rendering Intro: %v\n", err)
		}

		// Render Scenes
		for i, itemScript := range scriptData.Items {
			if i >= len(scenePaths) { break }
			segPath := fmt.Sprintf("output/seg_%d.mp4", i)
			if err := renderSegment(itemScript, scenePaths[i], segPath, videoType); err == nil {
				segmentFiles = append(segmentFiles, segPath)
			} else {
				fmt.Printf("❌ Error Rendering Scene %d: %v\n", i, err)
			}
		}

		// Render Outro
		outroVid := "output/seg_outro.mp4"
		if err := renderSegment(scriptData.Outro, outroPath, outroVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, outroVid)
		}

		// --- 6. STITCH FINAL MOVIE ---
		fmt.Println("🔹 STEP 4: Stitching Video...") // DEBUG
		finalVideo := "output/final_movie.mp4"
		if err := stitchVideos(segmentFiles, finalVideo); err != nil {
			fmt.Printf("❌ CRITICAL ERROR (Stitch): %v\n", err) // DEBUG
			c.JSON(500, gin.H{"error": "Stitch failed: " + err.Error()})
			return
		}

		// Success!
		fmt.Println("✅ SUCCESS! Video Ready.") // DEBUG
		scheme := "http"
		if c.Request.TLS != nil || c.Request.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		videoUrl := fmt.Sprintf("%s://%s/videos/final_movie.mp4", scheme, c.Request.Host)

		c.JSON(200, gin.H{"status": "success", "video_url": videoUrl})
	})

	// Ensure output directory exists
	if _, err := os.Stat("output"); os.IsNotExist(err) { os.Mkdir("output", 0755) }

	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	fmt.Println("🚀 Server running on port " + port)
	r.Run(":" + port)
}

// --- 1. AI BRAIN (Uses Groq) ---
func generateSegmentedScript(topic, category, videoType string, scenes []SceneData) (ScriptResponse, error) {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" { return ScriptResponse{}, fmt.Errorf("missing GROQ_API_KEY") }

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = "https://api.groq.com/openai/v1"
	client := openai.NewClientWithConfig(config)

	itemsContext := ""
	for i, s := range scenes {
		name := s.Name
		if name == "" { name = fmt.Sprintf("Item %d", i+1) }
		itemsContext += fmt.Sprintf("\nItem %d: %s\nDetails: %s\n", i+1, name, s.Details)
	}

	prompt := fmt.Sprintf(`
	Topic: "%s" (%s mode)
	Create a video script.
	INPUT ITEMS:
	%s

	RETURN JSON ONLY:
	{
		"intro": "Hook",
		"items": ["Script 1", "Script 2"],
		"outro": "Conclusion"
	}
	`, topic, videoType, itemsContext)

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: "llama-3.3-70b-versatile",
			Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: prompt}},
			ResponseFormat: &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject},
		},
	)
	if err != nil { return ScriptResponse{}, err }

	var result ScriptResponse
	clean := strings.ReplaceAll(resp.Choices[0].Message.Content, "```json", "")
	clean = strings.ReplaceAll(clean, "```", "")

	if err := json.Unmarshal([]byte(clean), &result); err != nil {
		return ScriptResponse{}, fmt.Errorf("json parse error: %v", err)
	}
	return result, nil
}

// --- 2. RENDER ENGINE (Memory Optimized + Free TTS) ---
func renderSegment(text, mediaPath, outputPath, videoType string) error {
	// 1. Generate Audio using Google TTS (Free)
	audioPath := strings.Replace(outputPath, ".mp4", ".mp3", 1)

	// Call the Helper
	err := downloadGoogleTTS(text, audioPath)
	if err != nil {
		return fmt.Errorf("Google TTS failed: %v", err)
	}

	// Clean up old video file if exists
	os.Remove(outputPath)

	// 2. Set Resolution
	scale := "scale=1080:1920:force_original_aspect_ratio=decrease,pad=1080:1920:(ow-iw)/2:(oh-ih)/2,format=yuv420p" // Short
	if videoType == "long" {
		scale = "scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:(ow-iw)/2:(oh-ih)/2,format=yuv420p" // Long
	}

	// 3. Render
	ext := strings.ToLower(filepath.Ext(mediaPath))
	isVideo := ext == ".mp4" || ext == ".mov" || ext == ".avi"
	var cmd *exec.Cmd

	if isVideo {
		// Video Logic: Low Memory Mode
		cmd = exec.Command("ffmpeg",
			"-stream_loop", "-1", "-i", mediaPath, // Video input
			"-i", audioPath,                       // Audio input
			"-map", "0:v", "-map", "1:a",
			"-vf", scale,
			"-threads", "1",             // <--- LIMIT THREADS (Saves RAM)
			"-c:v", "libx264", 
			"-preset", "ultrafast",      // <--- FASTER & LESS RAM
			"-c:a", "aac", "-b:a", "128k",
			"-shortest",
			outputPath,
		)
	} else {
		// Image Logic: Low Memory Mode
		cmd = exec.Command("ffmpeg",
			"-loop", "1", "-i", mediaPath, // Image input
			"-i", audioPath,               // Audio input
			"-vf", scale,
			"-threads", "1",             // <--- LIMIT THREADS
			"-c:v", "libx264", 
			"-tune", "stillimage",
			"-preset", "ultrafast",      // <--- FASTER & LESS RAM
			"-c:a", "aac", "-b:a", "128k",
			"-shortest",
			outputPath,
		)
	}

	output, err := cmd.CombinedOutput()
	os.Remove(audioPath) // Cleanup audio

	if err != nil {
		fmt.Printf("❌ FFmpeg Error: %s\n", string(output))
		return err
	}
	return nil
}

// --- NEW HELPER: GOOGLE TTS (FREE) ---
func downloadGoogleTTS(text, outFile string) error {
	// Google TTS URL (Unofficial but widely used)
	safeText := url.QueryEscape(text)
	// Truncate text if too long to avoid URL limits (basic safety)
	if len(safeText) > 1000 { safeText = safeText[:1000] }
	
	ttsUrl := fmt.Sprintf("https://translate.google.com/translate_tts?ie=UTF-8&q=%s&tl=en&client=tw-ob", safeText)

	req, _ := http.NewRequest("GET", ttsUrl, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0") // Pretend to be a browser

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Google TTS rejected request: %d", resp.StatusCode)
	}

	out, err := os.Create(outFile)
	if err != nil { return err }
	defer out.Close()

	io.Copy(out, resp.Body)
	return nil
}

// --- 3. STITCHER ---
func stitchVideos(files []string, outputFile string) error {
	listFile, _ := os.Create("output/list.txt")
	for _, f := range files {
		listFile.WriteString(fmt.Sprintf("file '%s'\n", filepath.Base(f)))
	}
	listFile.Close()

	os.Remove(outputFile)
	// Run from current directory
	cmd := exec.Command("ffmpeg", "-f", "concat", "-safe", "0", "-i", "output/list.txt", "-c", "copy", outputFile)
	return cmd.Run()
}

// --- 4. HELPERS ---
func downloadTMDBPoster(query string, destinationPath string) error {
	apiKey := os.Getenv("TMDB_API_KEY")
	if apiKey == "" { apiKey = os.Getenv("TMDB_API_TOKEN") }
	if apiKey == "" { return fmt.Errorf("missing API key") }

	safeQuery := url.QueryEscape(query)
	searchUrl := fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s&query=%s&include_adult=false", apiKey, safeQuery)

	req, _ := http.NewRequest("GET", searchUrl, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer res.Body.Close()

	if res.StatusCode != 200 { return fmt.Errorf("status %d", res.StatusCode) }

	var result TMDBSearchResponse
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil { return err }

	if len(result.Results) == 0 { return fmt.Errorf("not found") }

	posterUrl := "https://image.tmdb.org/t/p/original" + result.Results[0].PosterPath
	return downloadFile(posterUrl, destinationPath)
}

func downloadPlaceholder(text, destinationPath, videoType string) {
	dims := "1080x1920"
	if videoType == "long" { dims = "1920x1080" }

	safe := url.QueryEscape(text)
	url := fmt.Sprintf("https://placehold.co/%s/111/FFF/png?text=%s", dims, safe)
	downloadFile(url, destinationPath)
}

func downloadFile(urlStr, destinationPath string) error {
	resp, err := http.Get(urlStr)
	if err != nil { return err }
	defer resp.Body.Close()
	out, err := os.Create(destinationPath)
	if err != nil { return err }
	defer out.Close()
	io.Copy(out, resp.Body)
	return nil
}