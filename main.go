package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	_ = godotenv.Load()

	r := gin.Default()
	r.Static("/videos", "./output")
	r.MaxMultipartMemory = 100 << 20

	r.POST("/generate-multi-scene", func(c *gin.Context) {
		fmt.Println("\n🔹 STEP 1: Request Received")

		topic := c.PostForm("topic")
		category := c.PostForm("category")
		videoType := c.PostForm("type")
		scenesJson := c.PostForm("scenes")

		if videoType == "" { videoType = "short" }

		var scenes []SceneData
		if err := json.Unmarshal([]byte(scenesJson), &scenes); err != nil {
			fmt.Println("❌ Error: Invalid JSON")
			c.JSON(400, gin.H{"error": "Invalid scenes JSON"})
			return
		}

		fmt.Printf("🎬 Topic: %s | Mode: %s | Items: %d\n", topic, videoType, len(scenes))

		// --- HELPER: SAVE MEDIA ---
		saveMedia := func(formKey, fallbackName string, tryTMDB bool) string {
			file, err := c.FormFile(formKey)
			if err == nil {
				ext := filepath.Ext(file.Filename)
				if ext == "" { ext = ".jpg" }
				savePath := fmt.Sprintf("output/%s%s", formKey, ext)
				c.SaveUploadedFile(file, savePath)
				return savePath
			}

			savePath := fmt.Sprintf("output/%s.jpg", formKey)
			if tryTMDB && category == "movie" && fallbackName != "" {
				if err := downloadTMDBPoster(fallbackName, savePath); err == nil {
					return savePath
				}
			}

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

		// --- AI SCRIPT ---
		fmt.Println("🔹 STEP 2: Generating Script (Groq)...")
		scriptData, err := generateSegmentedScript(topic, category, videoType, scenes)
		if err != nil {
			fmt.Printf("❌ CRITICAL ERROR (Groq): %v\n", err)
			c.JSON(500, gin.H{"error": "AI Script failed: " + err.Error()})
			return
		}
		
		// SAFETY CHECK: Ensure we have enough script items for the scenes
		if len(scriptData.Items) < len(scenes) {
		    fmt.Println("⚠️ Warning: AI returned fewer items than requested. Padding with generic text.")
		    for len(scriptData.Items) < len(scenes) {
		        scriptData.Items = append(scriptData.Items, "Here is another amazing item.")
		    }
		}

		// --- RENDER ---
		fmt.Println("🔹 STEP 3: Rendering Segments...")
		var segmentFiles []string

		// Render Intro
		introVid := "output/seg_intro.mp4"
		if err := renderSegment(scriptData.Intro, introPath, introVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, introVid)
		}

		// Render Scenes
		for i, itemScript := range scriptData.Items {
			if i >= len(scenePaths) { break }
			segPath := fmt.Sprintf("output/seg_%d.mp4", i)
			// Use the simpler plain text script
			if err := renderSegment(itemScript, scenePaths[i], segPath, videoType); err == nil {
				segmentFiles = append(segmentFiles, segPath)
			}
		}

		// Render Outro
		outroVid := "output/seg_outro.mp4"
		if err := renderSegment(scriptData.Outro, outroPath, outroVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, outroVid)
		}

		// --- STITCH ---
		fmt.Println("🔹 STEP 4: Stitching Video...")
		finalVideo := "output/final_movie.mp4"
		if err := stitchVideos(segmentFiles, finalVideo); err != nil {
			fmt.Printf("❌ CRITICAL ERROR (Stitch): %v\n", err)
			c.JSON(500, gin.H{"error": "Stitch failed: " + err.Error()})
			return
		}

		fmt.Println("✅ SUCCESS! Video Ready.")
		scheme := "http"
		if c.Request.TLS != nil || c.Request.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		videoUrl := fmt.Sprintf("%s://%s/videos/final_movie.mp4", scheme, c.Request.Host)

		c.JSON(200, gin.H{"status": "success", "video_url": videoUrl})
	})

	if _, err := os.Stat("output"); os.IsNotExist(err) { os.Mkdir("output", 0755) }
	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	fmt.Println("🚀 Server running on port " + port)
	r.Run(":" + port)
}

// --- 1. AI BRAIN (UPDATED PROMPT) ---
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

	// UPDATED PROMPT: Forces strictly simple strings
	prompt := fmt.Sprintf(`
	Topic: "%s" (%s mode)
	Create a video script for %d items.
	
	INPUT ITEMS:
	%s

	IMPORTANT JSON RULES:
	1. "items" MUST be a simple list of strings. 
	2. Do NOT use objects inside "items". 
	3. Example: ["This is item 1 script.", "This is item 2 script."]
	4. Do NOT include titles like "Item 1: ...", just the spoken script.

	RETURN JSON ONLY:
	{
		"intro": "Hook",
		"items": ["Script for Item 1", "Script for Item 2", "Script for Item 3..."],
		"outro": "Conclusion"
	}
	`, topic, videoType, len(scenes), itemsContext)

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
		return ScriptResponse{}, fmt.Errorf("json parse error: %v. Raw AI: %s", err, clean)
	}
	return result, nil
}

// --- 2. RENDER ENGINE (Free TTS + Optimized) ---
func renderSegment(text, mediaPath, outputPath, videoType string) error {
	audioPath := strings.Replace(outputPath, ".mp4", ".mp3", 1)
	if err := downloadGoogleTTS(text, audioPath); err != nil {
		return fmt.Errorf("Google TTS failed: %v", err)
	}
	os.Remove(outputPath)

	scale := "scale=1080:1920:force_original_aspect_ratio=decrease,pad=1080:1920:(ow-iw)/2:(oh-ih)/2,format=yuv420p"
	if videoType == "long" {
		scale = "scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080:(ow-iw)/2:(oh-ih)/2,format=yuv420p"
	}

	ext := strings.ToLower(filepath.Ext(mediaPath))
	isVideo := ext == ".mp4" || ext == ".mov" || ext == ".avi"
	var cmd *exec.Cmd

	if isVideo {
		cmd = exec.Command("ffmpeg", "-stream_loop", "-1", "-i", mediaPath, "-i", audioPath,
			"-map", "0:v", "-map", "1:a", "-vf", scale, "-threads", "1",
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "128k", "-shortest", outputPath)
	} else {
		cmd = exec.Command("ffmpeg", "-loop", "1", "-i", mediaPath, "-i", audioPath,
			"-vf", scale, "-threads", "1",
			"-c:v", "libx264", "-tune", "stillimage", "-preset", "ultrafast", "-c:a", "aac", "-b:a", "128k", "-shortest", outputPath)
	}

	output, err := cmd.CombinedOutput()
	os.Remove(audioPath)
	if err != nil {
		fmt.Printf("❌ FFmpeg Error: %s\n", string(output))
		return err
	}
	return nil
}

func downloadGoogleTTS(text, outFile string) error {
	safeText := url.QueryEscape(text)
	if len(safeText) > 1000 { safeText = safeText[:1000] }
	ttsUrl := fmt.Sprintf("https://translate.google.com/translate_tts?ie=UTF-8&q=%s&tl=en&client=tw-ob", safeText)

	req, _ := http.NewRequest("GET", ttsUrl, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	if resp.StatusCode != 200 { return fmt.Errorf("Google TTS rejected: %d", resp.StatusCode) }
	out, err := os.Create(outFile)
	if err != nil { return err }
	defer out.Close()
	io.Copy(out, resp.Body)
	return nil
}

func stitchVideos(files []string, outputFile string) error {
	listFile, _ := os.Create("output/list.txt")
	for _, f := range files { listFile.WriteString(fmt.Sprintf("file '%s'\n", filepath.Base(f))) }
	listFile.Close()
	os.Remove(outputFile)
	return exec.Command("ffmpeg", "-f", "concat", "-safe", "0", "-i", "output/list.txt", "-c", "copy", outputFile).Run()
}

func downloadTMDBPoster(query string, dest string) error {
	apiKey := os.Getenv("TMDB_API_KEY")
	if apiKey == "" { apiKey = os.Getenv("TMDB_API_TOKEN") }
	if apiKey == "" { return fmt.Errorf("missing key") }
	safe := url.QueryEscape(query)
	url := fmt.Sprintf("https://api.themoviedb.org/3/search/movie?api_key=%s&query=%s&include_adult=false", apiKey, safe)
	resp, err := http.Get(url)
	if err != nil { return err }
	defer resp.Body.Close()
	var res TMDBSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil { return err }
	if len(res.Results) == 0 { return fmt.Errorf("not found") }
	return downloadFile("https://image.tmdb.org/t/p/original"+res.Results[0].PosterPath, dest)
}

func downloadPlaceholder(text, dest, vType string) {
	dims := "1080x1920"
	if vType == "long" { dims = "1920x1080" }
	safe := url.QueryEscape(text)
	downloadFile(fmt.Sprintf("https://placehold.co/%s/111/FFF/png?text=%s", dims, safe), dest)
}

func downloadFile(urlStr, dest string) error {
	resp, err := http.Get(urlStr)
	if err != nil { return err }
	defer resp.Body.Close()
	out, err := os.Create(dest)
	if err != nil { return err }
	defer out.Close()
	io.Copy(out, resp.Body)
	return nil
}