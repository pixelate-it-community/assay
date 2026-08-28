package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/pixelate-it/molten/canvas"
	"github.com/pixelate-it/molten/format"

	//"runtime"
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
	patternsPath := fs.String("patterns", "zapret.json", "path to forbidden patterns JSON file")
	liveMode := fs.Bool("live", false, "watch file continuously in live mode")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "error: -in flag is required")
		return errUsage
	}

	// 1. Загрузка паттернов
	var err error
	forbiddenPatterns, err = loadPatterns(*patternsPath)
	if err != nil {
		return fmt.Errorf("failed to load patterns: %w", err)
	}
	log.Printf("[INFO] Loaded %d forbidden patterns from '%s'", len(forbiddenPatterns), *patternsPath)

	// 2. Открытие файла или стрима
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

	// 3. Создание ридера и чтение Header + первого Resize
	reader := format.NewChunkReader(bufio.NewReaderSize(r, 64*1024))

	// В v0.2.0 ReadHeader возвращает (header, openingResize, err)
	// Назовем переменную openingWindow, так как это уже готовый формат окна
	_, openingWindow, _, err := format.ReadHeader(reader)
	if err != nil {
		return fmt.Errorf("read header error: %w", err)
	}

	// Инициализируем виртуальный холст через Reframe, сразу передавая окно
	state := canvas.NewState()
	state.Reframe(openingWindow)

	// Так как это обычная Go-структура, обращаемся к полям напрямую
	width := int(openingWindow.Width)
	height := int(openingWindow.Height)

	if *liveMode {
		log.Printf("[LIVE MODE] Started pattern detection on canvas %dx%d...", width, height)
	} else {
		log.Printf("[STATIC MODE] Analyzing canvas %dx%d...", width, height)
	}

	var detectionsCount int

	// 4. Основной цикл чтения чанков
	for {
		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if *liveMode {
					time.Sleep(1000 * time.Millisecond)
					continue
				}
				break
			}
			return fmt.Errorf("read chunk error: %w", err)
		}

		switch {
		// Реакция на изменение размеров холста (вместо Keyframe)
		case chunk.GetResize() != nil:
			resizeChunk := chunk.GetResize()
			width = int(resizeChunk.GetWidth())
			height = int(resizeChunk.GetHeight())

			// Встроенный метод canvas.State сам перенесет пиксели на новое окно!
			state.ApplyResize(resizeChunk)
			log.Printf("[LIVE] Canvas resized to: %dx%d", width, height)

		// Реакция на новые пиксели
		case chunk.GetDelta() != nil:
			delta := chunk.GetDelta()

			// Применяем изменения к состоянию холста
			state.ApplyDelta(delta)

			ordered := format.PlacementOrder(0, delta)
			for _, p := range ordered {
				if p.Color == 0xFFFFFF {
					continue
				}

				// НОВОЕ: Берем готовые X и Y напрямую из пикселя!
				x := int(p.GetX())
				y := int(p.GetY())

				if checkPatternsAround(state, width, height, x, y, p.Color) {
					detectionsCount++
				}
			}

		// Завершение записи (сезон окончен)
		case chunk.GetFooter() != nil:
			log.Println("[INFO] Reached recording footer. Analysis finished.")
			break
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
						log.Printf("[!] DETECTED PATTERN: '%s' | Coords: (%d, %d) | Color: #%06X\n",
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
	// Погрешность цвета (квадрат расстояния RGB).
	// 0 = точное совпадение, 1000-2000 = легкие оттенки, 5000+ = сильный разброс.
	tolerance := 1500

	for r := 0; r < len(matrix); r++ {
		for c := 0; c < len(matrix[0]); c++ {
			expected := matrix[r][c]
			if expected == -1 {
				continue // -1 пропускаем, тут может быть что угодно
			}

			px := startX + c
			py := startY + r

			// Проверка выхода за границы холста
			if px < 0 || py < 0 || px >= width || py >= height {
				return false
			}

			currentColor := getColorAt(state, px, py)

			// Проверяем, похож ли цвет на холсте на цвет нашей искомой фигуры
			isTarget := isSimilarColor(currentColor, targetColor, tolerance)

			if expected == 1 {
				// Здесь должна быть часть фигуры. Если цвет не похож — это не наша фигура.
				if !isTarget {
					return false
				}
			} else if expected == 0 {
				// Здесь должен быть фон. Главное правило: фон НЕ должен сливаться с фигурой.
				if isTarget {
					return false
				}
			}
		}
	}

	return true
}

// isSimilarColor вычисляет разницу между двумя HEX-цветами
func isSimilarColor(c1, c2 uint32, tolerance int) bool {
	// Разбиваем первый цвет на R, G, B
	r1 := int((c1 >> 16) & 0xFF)
	g1 := int((c1 >> 8) & 0xFF)
	b1 := int(c1 & 0xFF)

	// Разбиваем второй цвет на R, G, B
	r2 := int((c2 >> 16) & 0xFF)
	g2 := int((c2 >> 8) & 0xFF)
	b2 := int(c2 & 0xFF)

	// Вычисляем квадрат расстояния между цветами в пространстве RGB
	dist := (r1-r2)*(r1-r2) + (g1-g2)*(g1-g2) + (b1-b2)*(b1-b2)

	return dist <= tolerance
}
