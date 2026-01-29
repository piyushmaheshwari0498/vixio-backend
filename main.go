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

type ScriptItem struct {
	Title   string `json:"title"`
	Details string `json:"details"`
}

type ScriptResponse struct {
	Intro string       `json:"intro"`
	Items []ScriptItem `json:"items"`
	Outro string       `json:"outro"`
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
		
		videoType := strings.ToLower(strings.TrimSpace(c.PostForm("type")))
		if videoType == "" { videoType = "short" }

		scenesJson := c.PostForm("scenes")
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

		// Padding check
		if len(scriptData.Items) < len(scenes) {
			for len(scriptData.Items) < len(scenes) {
				scriptData.Items = append(scriptData.Items, ScriptItem{Title: "Extra Item", Details: "Here is another item."})
			}
		}

		// --- RENDER ---
		fmt.Println("🔹 STEP 3: Rendering Segments...")
		var segmentFiles []string

		// Render Intro
		introVid := "output/seg_intro.mp4"
		if err := renderSegment(scriptData.Intro, introPath, introVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, introVid)
		} else {
			fmt.Printf("⚠️ Warning: Intro render failed: %v\n", err)
		}

		// Render Scenes
		for i, item := range scriptData.Items {
			if i >= len(scenePaths) { break }
			segPath := fmt.Sprintf("output/seg_%d.mp4", i)
			textToSpeak := item.Details

			if err := renderSegment(textToSpeak, scenePaths[i], segPath, videoType); err == nil {
				segmentFiles = append(segmentFiles, segPath)
			} else {
				fmt.Printf("⚠️ Warning: Scene %d failed: %v\n", i, err)
			}
		}

		// Render Outro
		outroVid := "output/seg_outro.mp4"
		if err := renderSegment(scriptData.Outro, outroPath, outroVid, videoType); err == nil {
			segmentFiles = append(segmentFiles, outroVid)
		} else {
			fmt.Printf("⚠️ Warning: Outro render failed: %v\n", err)
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

// --- 1. AI BRAIN ---
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

	// lengthInstruction := "Keep it snappy (1-2 sentences per item)."
	// if videoType == "long" {
	// 	lengthInstruction = "Write a detailed explanation (3-4 sentences per item)."
	// }

	// prompt := fmt.Sprintf(`
	// Topic: "%s" (%s mode)
	// Constraint: %s
	// INPUT ITEMS:
	// %s
	// RETURN JSON ONLY:
	// {
	// 	"intro": "Hook",
	// 	"items": [
	// 		{ "title": "Item 1 Title", "details": "Spoken text for item 1..." },
	// 		{ "title": "Item 2 Title", "details": "Spoken text for item 2..." }
	// 	],
	// 	"outro": "Conclusion"
	// }
	// `, topic, videoType, lengthInstruction, itemsContext)

	lengthInstruction := "Keep it snappy (max 25 words per item)."
    if videoType == "long" {
        lengthInstruction = "Write a professional detailed explanation (max 60 words per item)."
    }

    prompt := fmt.Sprintf(`
    Topic: "%s" (%s mode)
    Tone: Engaging and clear.
    Constraint: %s. Use simple language.
    INPUT ITEMS:
    %s
    RETURN JSON ONLY:
    {
        "intro": "Short hook",
        "items": [
            { "title": "Title", "details": "Script text..." }
        ],
        "outro": "Closing statement"
    }
    `, topic, videoType, lengthInstruction, itemsContext)

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
		fmt.Printf("❌ Failed JSON: %s\n", clean)
		return ScriptResponse{}, fmt.Errorf("json parse error")
	}
	return result, nil
}

// --- 2. RENDER ENGINE ---
func renderSegment(text, mediaPath, outputPath, videoType string) error {
	audioPath := strings.Replace(outputPath, ".mp4", ".mp3", 1)
	
	// FIX: Use the CHUNKED downloader
	if err := downloadGoogleTTS_Smart(text, audioPath); err != nil {
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
			"-map", "0:v", "-map", "1:a", 
			"-vf", scale, 
			"-r", "30",                 
			"-threads", "1", 
			"-c:v", "libx264", "-preset", "ultrafast", 
			"-c:a", "aac", "-b:a", "128k", 
			"-shortest", outputPath)
	} else {
		cmd = exec.Command("ffmpeg", "-loop", "1", "-i", mediaPath, "-i", audioPath,
			"-vf", scale, 
			"-r", "30",                 
			"-threads", "1",
			"-c:v", "libx264", "-tune", "stillimage", "-preset", "ultrafast", 
			"-c:a", "aac", "-b:a", "128k", 
			"-shortest", outputPath)
	}

	output, err := cmd.CombinedOutput()
	os.Remove(audioPath)
	if err != nil {
		fmt.Printf("❌ FFmpeg Error: %s\n", string(output))
		return err
	}
	return nil
}

// --- 3. STITCHER ---
func stitchVideos(files []string, outputFile string) error {
	if len(files) == 0 { return fmt.Errorf("no video segments were created") }
	listFile, _ := os.Create("output/list.txt")
	for _, f := range files {
		absPath, _ := filepath.Abs(f)
		listFile.WriteString(fmt.Sprintf("file '%s'\n", absPath))
	}
	listFile.Close()
	os.Remove(outputFile) 
	cmd := exec.Command("ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", "output/list.txt", "-c", "copy", outputFile)
	output, err := cmd.CombinedOutput()
	if err != nil { return fmt.Errorf("Stitch Error: %v | Log: %s", err, string(output)) }
	return nil
}

// --- 4. HELPERS (SMART CHUNKED TTS) ---
// func downloadGoogleTTS_Smart(text, outFile string) error {
// 	// 1. Create the final file
// 	finalFile, err := os.Create(outFile)
// 	if err != nil { return err }
// 	defer finalFile.Close()

// 	// 2. Split text into sentences to avoid "400 Bad Request"
// 	// We split by "." and "!" and "?"
// 	// Simple approach: Replace ! and ? with . then split by .
// 	cleanText := strings.ReplaceAll(text, "!", ".")
// 	cleanText = strings.ReplaceAll(cleanText, "?", ".")
// 	sentences := strings.Split(cleanText, ".")

// 	for _, sentence := range sentences {
// 		sentence = strings.TrimSpace(sentence)
// 		if len(sentence) < 2 { continue } // Skip empty junk

// 		// 3. Download this sentence
// 		safeText := url.QueryEscape(sentence)
// 		ttsUrl := fmt.Sprintf("https://translate.googleapis.com/translate_tts?client=gtx&ie=UTF-8&tl=en&dt=t&q=%s", safeText)

// 		req, _ := http.NewRequest("GET", ttsUrl, nil)
// 		req.Header.Set("User-Agent", "Mozilla/5.0")
		
// 		resp, err := http.DefaultClient.Do(req)
// 		if err != nil { 
// 			fmt.Println("TTS Network Error:", err)
// 			continue 
// 		}
		
// 		if resp.StatusCode != 200 {
// 			fmt.Printf("TTS Error: %d for '%s'\n", resp.StatusCode, sentence)
// 			resp.Body.Close()
// 			continue
// 		}

// 		// 4. Append MP3 bytes to the final file
// 		// (MP3s can be concatenated just by appending bytes!)
// 		io.Copy(finalFile, resp.Body)
// 		resp.Body.Close()
// 	}
	
// 	return nil
// }
func downloadGoogleTTS_Smart(text, outFile string) error {
    finalFile, err := os.Create(outFile)
    if err != nil { return err }
    defer finalFile.Close()

    // Split text into chunks of max 180 characters to stay safe with Google TTS
    chunks := splitText(text, 180)

    for _, chunk := range chunks {
        chunk = strings.TrimSpace(chunk)
        if len(chunk) < 2 { continue }

        safeText := url.QueryEscape(chunk)
        ttsUrl := fmt.Sprintf("https://translate.googleapis.com/translate_tts?client=gtx&ie=UTF-8&tl=en&dt=t&q=%s", safeText)

        req, _ := http.NewRequest("GET", ttsUrl, nil)
        req.Header.Set("User-Agent", "Mozilla/5.0")
        
        resp, err := http.DefaultClient.Do(req)
        if err != nil { continue }
        
        if resp.StatusCode == 200 {
            io.Copy(finalFile, resp.Body)
        }
        resp.Body.Close()
    }
    return nil
}

// Helper to ensure text segments don't break the API
func splitText(text string, limit int) []string {
    var chunks []string
    // Split by common sentence endings first
    sentences := strings.FieldsFunc(text, func(r rune) bool {
        return r == '.' || r == '!' || r == '?'
    })

    for _, s := range sentences {
        s = strings.TrimSpace(s)
        if len(s) <= limit {
            chunks = append(chunks, s)
        } else {
            // If a single sentence is still too long, break it by spaces
            words := strings.Split(s, " ")
            currentChunk := ""
            for _, word := range words {
                if len(currentChunk)+len(word) < limit {
                    currentChunk += " " + word
                } else {
                    chunks = append(chunks, strings.TrimSpace(currentChunk))
                    currentChunk = word
                }
            }
            chunks = append(chunks, strings.TrimSpace(currentChunk))
        }
    }
    return chunks
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