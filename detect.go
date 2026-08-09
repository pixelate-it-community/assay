package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"
	"github.com/pixelate-it/molten/canvas"
	"github.com/pixelate-it/molten/format"
	"runtime"
	"net/http"
	"strings"
)

//test memory usage
//func printMemUsage() {
//	var m runtime.MemStats
//	runtime.ReadMemStats(&m)
//	fmt.Printf("[MEM] Mem : %v MB | Vsego videlal: %v MB\n", m.Alloc/1024/1024, m.TotalAlloc/1024/1024)
//}
//

var errUsage = fmt.Errorf("invalid arguments")

type Pattern struct {
	Name   string  `json:"name"`
	Matrix [][]int `json:"matrix"`
}
var forbiddenPatterns []Pattern

func loadPatterns(path string) ([]Pattern, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading pattern file: %w", err)
	}

	var patterns []Pattern
	if err := json.Unmarshal(data, &patterns); err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	return patterns, nil
}

func getColorAt(state *canvas.State, x, y int) uint32 {
	img := state.Image()
	if img == nil {
		return 0xFFFFFF
	}

	off := img.PixOffset(x, y)
	r := uint32(img.Pix[off+0])
	g := uint32(img.Pix[off+1])
	b := uint32(img.Pix[off+2])

	return (r << 16) | (g << 8) | b
}

func runDetect(args []string) error {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	inputPath := fs.String("in", "", "path to .mltn file (required)")
	//
	//
	// PATH zapretki. Mirdukkk Pri Peremeshenii ili izmenenenii faila ychti
	patternsPath := fs.String("patterns", "zapret.json", "path to forbidden patterns JSON file")
	//
	//
	//
	liveMode := fs.Bool("live", false, "watch file continuously in live mode")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "error: -in flag is required")
		return errUsage
	}

	// 1. Load forbidden patterns from JSON
	var err error
	forbiddenPatterns, err = loadPatterns(*patternsPath)
	if err != nil {
		return fmt.Errorf("failed to load patterns: %w", err)
	}
	log.Printf("[INFO] Loaded %d forbidden patterns from '%s'", len(forbiddenPatterns), *patternsPath)

	// 2. Open the canvas recording (local file or HTTP/HTTPS live stream)
	var r io.ReadCloser
	if strings.HasPrefix(*inputPath, "http://") || strings.HasPrefix(*inputPath, "https://") {
		resp, err := http.Get(*inputPath)
		if err != nil {
			return fmt.Errorf("failed to connect to server: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("server returned non-OK status: %s", resp.Status)
		}
		r = resp.Body
		log.Printf("[INFO] Connected to live HTTP stream: %s", *inputPath)
	} else {
		r, err = os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open file error: %w", err)
		}
		log.Printf("[INFO] Opened local file: %s", *inputPath)
	}
	defer r.Close()

	reader := format.NewChunkReader(bufio.NewReaderSize(r, 64*1024))
	_, initialKf, width, height, err := format.ReadHeader(reader)
	if err != nil {
		return fmt.Errorf("read header error: %w", err)
	}

	if *liveMode {
		log.Printf("[LIVE MODE] Started pattern detection on canvas %dx%d...", width, height)
	} else {
		log.Printf("[STATIC MODE] Analyzing canvas %dx%d...", width, height)
	}

	state := canvas.NewState()
	state.Resize(width, height)
	for _, p := range initialKf.Pixels {
		state.ApplySinglePixel(p)
	}

	var detectionsCount int

	for {
		chunk, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				if *liveMode {

					//time sleep for waiting new write
					//
					time.Sleep(10000 * time.Millisecond)
					//
					//
					continue
				}
				break
			}
			return fmt.Errorf("read chunk error: %w", err)
		}

		if kf := chunk.GetKeyframe(); kf != nil {
        
        width = kf.GetWidth()
		height = kf.GetHeight()
        for _, p := range kf.Pixels {
            state.ApplySinglePixel(p)
        }

        log.Printf("[LIVE] new size: %dx%d", width, height)
        continue 
		}
	
		if chunk.GetDelta() != nil {
			ordered, _ := format.PlacementOrder(chunk.GetDelta())

			for _, p := range ordered {		

				state.ApplySinglePixel(p)

				if p.Color == 0xFFFFFF {
					continue
				}

				x := int(p.Id) % int(width)
				y := int(p.Id) / int(width)

				if checkPatternsAround(state, int(width), int(height), x, y, p.Color) {
					detectionsCount++
					
				}
			}
			
		}
		
	}
	

	log.Printf("Analysis complete. Total matches found: %d\n", detectionsCount)
	return nil
}

func checkPatternsAround(state *canvas.State, width, height, x, y int, targetColor uint32) bool {
	detected := false

	for _, pat := range forbiddenPatterns {
		rows := len(pat.Matrix)
		cols := len(pat.Matrix[0])

		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				if pat.Matrix[r][c] == 1 {
					startX := x - c
					startY := y - r

					if matchPattern(state, width, height, startX, startY, pat.Matrix, targetColor) {
						fmt.Printf("[!] DETECTED PATTERN: '%s' | Coords: (%d, %d) | Color: #%06X\n",
							pat.Name, startX, startY, targetColor)
						detected = true
					}
				}
			}
		}
	}

	return detected
}

func matchPattern(state *canvas.State, width, height, startX, startY int, matrix [][]int, targetColor uint32) bool {
	var bgColor uint32
	hasBgColor := false

	for r := 0; r < len(matrix); r++ {
		for c := 0; c < len(matrix[0]); c++ {
			expected := matrix[r][c]
			if expected == -1 {
				continue
			}

			px := startX + c
			py := startY + r

			if px < 0 || py < 0 || px >= width || py >= height {
				return false
			}

			currentColor := getColorAt(state, px, py)

			if expected == 1 {
				if currentColor != targetColor {
					return false
				}
			} else if expected == 0 {
				if currentColor == targetColor {
					return false
				}

				if !hasBgColor {
					bgColor = currentColor
					hasBgColor = true
				} else if currentColor != bgColor {
					return false
				}
			}
		}
	}

	return true
}


