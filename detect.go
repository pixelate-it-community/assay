package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pixelate-it/molten/canvas"
	"github.com/pixelate-it/molten/format"
	"github.com/pixelate-it/molten/ore"
)

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

	// Сортировка: сначала фигуры с наибольшим количеством клеток 1
	sort.Slice(patterns, func(i, j int) bool {
		return countOnes(patterns[i].Matrix) > countOnes(patterns[j].Matrix)
	})

	return patterns, nil
}

func countOnes(matrix [][]int) int {
	cnt := 0
	for _, row := range matrix {
		for _, val := range row {
			if val == 1 {
				cnt++
			}
		}
	}
	return cnt
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
	patternsPath := fs.String("patterns", "zapret.json", "path to forbidden patterns JSON file")
	liveMode := fs.Bool("live", false, "watch file continuously in live mode")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "error: -in flag is required")
		return errUsage
	}

	var err error
	forbiddenPatterns, err = loadPatterns(*patternsPath)
	if err != nil {
		return fmt.Errorf("failed to load patterns: %w", err)
	}
	log.Printf("[INFO] Loaded %d forbidden patterns from '%s'", len(forbiddenPatterns), *patternsPath)

	// 2. Открытие файла или стрима
	var r io.ReadCloser
	var localFile *os.File

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
		localFile, err = os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open file error: %w", err)
		}
		r = localFile
		log.Printf("[INFO] Opened local file: %s", *inputPath)
	}
	defer r.Close()

	// 3. Чтение заголовка
	// ReadHeader требует *format.ChunkReader. Мы создаем его без bufio для live-файла,
	// чтобы не сбить системный указатель смещения (offset) внутри файла.
	var cr *format.ChunkReader
	if *liveMode && localFile != nil {
		cr = format.NewChunkReader(localFile)
	} else {
		cr = format.NewChunkReader(bufio.NewReaderSize(r, 64*1024))
	}

	_, openingWindow, _, err := format.ReadHeader(cr)
	if err != nil {
		return fmt.Errorf("read header error: %w", err)
	}

	// 4. Настройка ридера для основного цикла
	var reader interface {
		Next() (*ore.RecordingChunk, error)
	}

	if *liveMode && localFile != nil {
		reader = format.NewTailReader(localFile)
		log.Println("[INFO] Using format.TailReader for live file watching")
	} else {
		reader = cr
		if *liveMode {
			log.Println("[INFO] Using format.ChunkReader for live HTTP stream")
		}
	}

	state := canvas.NewState()
	state.Reframe(openingWindow)

	width := int(openingWindow.Width)
	height := int(openingWindow.Height)

	if *liveMode {
		log.Printf("[LIVE MODE] Started pattern detection on canvas %dx%d...", width, height)
	} else {
		log.Printf("[STATIC MODE] Analyzing canvas %dx%d...", width, height)
	}

	var detectionsCount int

	// Если не live-режим, мы считываем всё с выводом. Если live — сначала прогреваем холст молча.
	caughtUp := !*liveMode

	for {
		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if *liveMode {
					if !caughtUp {
						log.Println("[LIVE] История проиграна. Начинаю мониторинг новых данных...")
						caughtUp = true
					}
					time.Sleep(500 * time.Millisecond)
					continue
				}
				break
			}
			return fmt.Errorf("read chunk error: %w", err)
		}

		switch {
		case chunk.GetResize() != nil:
			resizeChunk := chunk.GetResize()
			width = int(resizeChunk.GetWidth())
			height = int(resizeChunk.GetHeight())

			state.ApplyResize(resizeChunk)

			// Логируем ресайз холста только когда догнали хвост (или в статике)
			if caughtUp {
				log.Printf("[LIVE] Canvas resized to: %dx%d", width, height)
			}

		case chunk.GetDelta() != nil:
			delta := chunk.GetDelta()

			// ВСЕГДА применяем дельту, чтобы виртуальный холст (state) был актуальным
			state.ApplyDelta(delta)

			// ИЩЕМ ПАТТЕРНЫ И ВЫВОДИМ ЛОГИ ТОЛЬКО ТОГДА, КОГДА ДОГНАЛИ ХВОСТ!
			if caughtUp {
				detectedThisChunk := make(map[string]bool)

				ordered := format.PlacementOrder(0, delta)
				for _, p := range ordered {
					if p.Color == 0xFFFFFF {
						continue
					}

					x := int(p.GetX())
					y := int(p.GetY())

					detectionsCount += checkPatternsAround(state, width, height, x, y, p.Color, detectedThisChunk)
				}
			}

		case chunk.GetFooter() != nil:
			// Если включен флаг -live, но мы встретили Footer — значит, запись уже закончена
			if *liveMode {
				return fmt.Errorf("error: -live flag cannot be used on a completed recording that contains a footer")
			}
			log.Println("[INFO] Reached recording footer. Analysis finished.")
			break
		}
	}

	log.Printf("Analysis complete. Total matches found: %d\n", detectionsCount)
	return nil
}

func checkPatternsAround(state *canvas.State, width, height, x, y int, targetColor uint32, detectedThisChunk map[string]bool) int {
	newDetections := 0

	for _, pat := range forbiddenPatterns {
		rows := len(pat.Matrix)
		cols := len(pat.Matrix[0])

		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				if pat.Matrix[r][c] == 1 {
					startX := x - c
					startY := y - r

					// Формируем уникальный ключ найденной фигуры
					key := fmt.Sprintf("%s:%d:%d", pat.Name, startX, startY)

					// Если уже находили эту фигуру в текущем Delta-чанке, проскакиваем
					if detectedThisChunk[key] {
						continue
					}

					if matchPattern(state, width, height, startX, startY, pat.Matrix, targetColor) {
						log.Printf("[!] DETECTED PATTERN: '%s' | Coords: (%d, %d) | Color: #%06X\n",
							pat.Name, startX, startY, targetColor)
						detectedThisChunk[key] = true
						newDetections++
					}
				}
			}
		}
	}

	return newDetections
}

func matchPattern(state *canvas.State, width, height, startX, startY int, matrix [][]int, targetColor uint32) bool {
	tolerance := 3500 // Оптимальный разброс цвета для шума

	totalOnePixels := 0
	matchedOnePixels := 0

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
			isTarget := isSimilarColor(currentColor, targetColor, tolerance)

			// Шаг 1: Если на месте 0 стоит цвет фигуры — это ДРУГАЯ (более крупная) фигура
			if expected == 0 && isTarget {
				return false
			}

			if expected == 1 {
				totalOnePixels++
				if isTarget {
					matchedOnePixels++
				}
			}
		}
	}

	if totalOnePixels == 0 {
		return false
	}

	// Шаг 3: Подняли порог до 85% совпадения
	return (float64(matchedOnePixels) / float64(totalOnePixels)) >= 0.85
}

func isSimilarColor(c1, c2 uint32, tolerance int) bool {
	r1 := int((c1 >> 16) & 0xFF)
	g1 := int((c1 >> 8) & 0xFF)
	b1 := int(c1 & 0xFF)

	r2 := int((c2 >> 16) & 0xFF)
	g2 := int((c2 >> 8) & 0xFF)
	b2 := int(c2 & 0xFF)

	dist := (r1-r2)*(r1-r2) + (g1-g2)*(g1-g2) + (b1-b2)*(b1-b2)

	return dist <= tolerance
}
