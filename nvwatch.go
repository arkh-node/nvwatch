package main

// nvwatch — сторож NVIDIA-рельса и писательницы zcode.
// Следит за газом API (пробы), статистика остановок (429/500/502/timeout),
// мягко будит писательницу при обрывах, буфер обрывов для контекста.
// TUI — в отдельном tmux-окне, глазурь: бары, цвета, история.

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------- конфиг ----------

type EventKind string

const (
	EvOK       EventKind = "OK"
	Ev429      EventKind = "429"
	Ev500      EventKind = "5xx"
	EvTimeout  EventKind = "timeout"
	EvWake     EventKind = "wake"
	EvHeart    EventKind = "heart"
	EvRSS      EventKind = "rss"
	EvBuffered EventKind = "buffer"
)

type Event struct {
	Time time.Time
	Kind EventKind
	Lat  float64 // мс
	Note string
}

// ---------- проба NVIDIA ----------

type probeResult struct {
	Code int
	Lat  float64 // мс
	Err  string
}

func readKey() string {
	data, err := os.ReadFile(os.Getenv("HOME") + "/.hermes/.env")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "NVIDIA_API_KEY=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "NVIDIA_API_KEY="))
		}
	}
	return ""
}

func probeNVIDIA(client *http.Client, key string) probeResult {
	body := []byte(`{"model":"z-ai/glm-5.3","max_tokens":4,"messages":[{"role":"user","content":"ок"}]}`)
	req, _ := http.NewRequest("POST", "https://integrate.api.nvidia.com/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	lat := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		return probeResult{Code: -1, Lat: lat, Err: err.Error()}
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	if code >= 500 {
		return probeResult{Code: code, Lat: lat}
	}
	if code == 429 {
		return probeResult{Code: 429, Lat: lat}
	}
	return probeResult{Code: code, Lat: lat}
}

// ---------- монитор TUI-сессии ----------

type uiState struct {
	Streaming   bool
	Dead        bool
	Context     string
	LastToolAge time.Duration
}

func capturePane() (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", "nevis_zcode:ohota", "-p").Output()
	return string(out), err
}

func spinnersAlive(pane string) bool {
	for _, s := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "Streaming", "model streaming"} {
		if strings.Contains(pane, s) {
			return true
		}
	}
	return false
}

// RSS zcode-cli в ГБ
func zcodeRSS() float64 {
	out, err := exec.Command("bash", "-c", `ps aux | awk '$11=="zcode-cli" {print $6}' | head -1`).Output()
	if err != nil {
		return 0
	}
	var kb float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &kb)
	return kb / 1048576.0
}

// ---------- будильник ----------

func wake(session string) {
	// мягкий пин: Enter в панель (пинает очередь)
	exec.Command("tmux", "send-keys", "-t", "nevis_zcode:"+session, "Enter").Run()
}

func wakeWithPrompt(session string, prompt string) {
	exec.Command("tmux", "send-keys", "-t", "nevis_zcode:"+session, "-l", prompt).Run()
	time.Sleep(300 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", "nevis_zcode:"+session, "Enter").Run()
}

// ---------- буфер ----------

type Buffer struct {
	mu   sync.Mutex
	ents []string
	path string
}

func (b *Buffer) Add(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ents = append(b.ents, time.Now().Format("15:04:05")+" | "+s)
	b.flush()
}

func (b *Buffer) flush() {
	if len(b.ents) == 0 {
		return
	}
	data := strings.Join(b.ents, "\n") + "\n"
	os.WriteFile(b.path, []byte(data), 0644)
}

// ---------- история для TUI ----------

type Ring struct {
	mu  sync.Mutex
	buf []Event
	max int
}

func (r *Ring) Add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, e)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

func (r *Ring) Last(n int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < n {
		return append([]Event{}, r.buf...)
	}
	return append([]Event{}, r.buf[len(r.buf)-n:]...)
}

// ---------- статистика ----------

type Stats struct {
	OK, E429, E5xx, Timeout int
	TotalLat               float64
	Wakes                  int
	mu                     sync.Mutex
}

func (s *Stats) record(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case EvOK:
		s.OK++
		s.TotalLat += e.Lat
	case Ev429:
		s.E429++
	case Ev500:
		s.E5xx++
	case EvTimeout:
		s.Timeout++
	case EvWake:
		s.Wakes++
	}
}

// ---------- TUI ----------

const (
	cReset = "\033[0m"
	cBold  = "\033[1m"
	cDim   = "\033[2m"
	cRed   = "\033[31m"
	cGreen = "\033[32m"
	cYell  = "\033[33m"
	cBlue  = "\033[34m"
	cMag   = "\033[35m"
	cCyan  = "\033[36m"
	cWhite = "\033[37m"
)

func clearScreen() { fmt.Print("\033[H\033[2J") }
func homeCursor()  { fmt.Print("\033[H") }
func hideCursor()  { fmt.Print("\033[?25l") }
func showCursor()  { fmt.Print("\033[?25h") }

func bar(pct float64, width int, color string) string {
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	return color + strings.Repeat("█", filled) + cDim + strings.Repeat("░", width-filled) + cReset
}

func render(stats *Stats, ring *Ring, rss float64, lastProbe probeResult, gas bool, bufN int) {
	homeCursor()
	fmt.Println(cBold + cMag + "  ⛤ NVWATCH" + cReset + cDim + " · сторож писательницы и газа NVIDIA" + cReset)
	fmt.Println(cDim + "  ─────────────────────────────────────────────────────────────" + cReset)

	// Газ
	status := cGreen + "ГОРИТ" + cReset
	if !gas {
		status = cRed + "ЗАКОНЧИЛСЯ" + cReset
	}
	fmt.Printf("  Газ: %s", status)
	if lastProbe.Code > 0 {
		fmt.Printf(cDim+"  (посл. проба: HTTP %d, %.0fмс)"+cReset, lastProbe.Code, lastProbe.Lat)
	}
	fmt.Println()

	// Пробный спарклайн (последние 20 событий)
	evs := ring.Last(20)
	fmt.Print("  Рельса: ")
	for _, e := range evs {
		switch e.Kind {
		case EvOK:
			fmt.Print(cGreen + "▁" + cReset)
		case Ev429:
			fmt.Print(cYell + "▄" + cReset)
		case Ev500:
			fmt.Print(cRed + "█" + cReset)
		case EvTimeout:
			fmt.Print(cRed + "▌" + cReset)
		case EvWake:
			fmt.Print(cCyan + "⚡" + cReset)
		case EvHeart:
			fmt.Print(cDim + "·" + cReset)
		}
	}
	fmt.Println()

	// Статистика
	fmt.Printf("  Пробы: %s%d OK%s | %s%d ×429%s | %s%d ×5xx%s | %s%d timeout%s",
		cGreen, stats.OK, cReset,
		cYell, stats.E429, cReset,
		cRed, stats.E5xx, cReset,
		cRed, stats.Timeout, cReset)
	if stats.OK > 0 {
		fmt.Printf(cDim+"  (ср. %.0fмс)"+cReset, stats.TotalLat/float64(stats.OK))
	}
	fmt.Printf("  | %s⚡ будильников: %d%s  |  %sбуфер: %d%s\n", cCyan, stats.Wakes, cReset, cBlue, bufN, cReset)

	// RSS писательницы
	rssPct := rss / 16.0
	rssCol := cGreen
	if rss > 12 {
		rssCol = cYell
	}
	if rss > 16 {
		rssCol = cRed
	}
	fmt.Printf("  Писательница RSS: %s%.1f ГБ%s %s\n", rssCol, rss, cReset, bar(minF(rssPct, 1.0), 30, rssCol))

	fmt.Println(cDim + "  ─────────────────────────────────────────────────────────────" + cReset)

	// Журнал
	fmt.Println(cBold + "  ЖУРНАЛ" + cReset)
	log := ring.Last(12)
	for _, e := range log {
		t := e.Time.Format("15:04:05")
		kindCol := cGreen
		note := e.Note
		switch e.Kind {
		case Ev429:
			kindCol = cYell
		case Ev500, EvTimeout:
			kindCol = cRed
		case EvWake:
			kindCol = cCyan
		case EvHeart:
			kindCol = cDim
		case EvRSS:
			kindCol = cMag
		case EvBuffered:
			kindCol = cBlue
		}
		fmt.Printf("  %s%s%s  %s%-8s%s  %s\n", cDim, t, cReset, kindCol, e.Kind, cReset, note)
	}
	fmt.Println(cDim + "  ─────────────────────────────────────────────────────────────" + cReset)
	fmt.Println(cDim + "  q — выход · проба каждые 30с · панель каждые 5с · будит при обрыве" + cReset)
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ---------- main ----------

func main() {
	session := flag.String("s", "0", "окно писательницы в nevis_zcode")
	probeEvery := flag.Int("probe", 30, "проба газа, сек")
	uiEvery := flag.Int("ui", 5, "обновление TUI, сек")
	bufferPath := flag.String("buffer", "/srv/run/nvwatch_buffer.txt", "файл буфера обрывов")
	flag.Parse()

	key := readKey()
	if key == "" {
		fmt.Println("нет NVIDIA_API_KEY")
		os.Exit(1)
	}
	client := &http.Client{Timeout: 20 * time.Second}

	stats := &Stats{}
	ring := &Ring{max: 100}
	buf := &Buffer{path: *bufferPath}

	// неблокирующее чтение 'q'
	quit := make(chan struct{})
	go func() {
		var b [1]byte
		for {
			os.Stdin.Read(b[:])
			if b[0] == 'q' {
				close(quit)
				return
			}
		}
	}()

	clearScreen()
	hideCursor()
	defer showCursor()

	lastProbe := probeResult{Code: -1}
	gas := true
	var uiMu sync.Mutex
	deadSince := time.Time{}
	consecutiveDead := 0

	lastProbeT := time.Now()
	lastUIT := time.Time{}
	lastHeartT := time.Time{}

	ring.Add(Event{Time: time.Now(), Kind: EvHeart, Note: "nvwatch запущен"})
	buf.Add("nvwatch: старт сессии наблюдения")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-quit:
			fmt.Println("\n  выход")
			return
		case <-ticker.C:
			now := time.Now()

			// --- газ: проба ---
			if now.Sub(lastProbeT) >= time.Duration(*probeEvery)*time.Second {
				lastProbeT = now
				res := probeNVIDIA(client, key)
				uiMu.Lock()
				lastProbe = res
				uiMu.Unlock()
				switch {
				case res.Code == 200:
					if !gas {
						gas = true
						buf.Add("nvwatch: газ вернулся (200)")
					}
					stats.record(Event{Kind: EvOK, Lat: res.Lat})
					ring.Add(Event{Time: now, Kind: EvOK, Note: fmt.Sprintf("%.0fмс", res.Lat)})
				case res.Code == 429:
					gas = false
					stats.record(Event{Kind: Ev429})
					ring.Add(Event{Time: now, Kind: Ev429, Note: "квота/лимит"})
					buf.Add("nvwatch: 429 — квота; писательница приостановлена")
				case res.Code >= 500 || res.Code == -1:
					gas = true // 5xx — не конец газа
					if res.Code == -1 {
						stats.record(Event{Kind: EvTimeout})
						ring.Add(Event{Time: now, Kind: EvTimeout, Note: "timeout"})
					} else {
						stats.record(Event{Kind: Ev500})
						ring.Add(Event{Time: now, Kind: Ev500, Note: fmt.Sprintf("HTTP %d", res.Code)})
					}
				default:
					if res.Code > 0 {
						ring.Add(Event{Time: now, Kind: Ev500, Note: fmt.Sprintf("HTTP %d", res.Code)})
					}
				}
			}

			// --- писательница: панель ---
			pane, err := capturePane()
			alive := false
			if err == nil {
				alive = spinnersAlive(pane)
			}
			if !alive {
				if deadSince.IsZero() {
					deadSince = now
					consecutiveDead = 0
				} else if now.Sub(deadSince) > 60*time.Second {
					consecutiveDead++
					// --- мягкий будильник ---
					if consecutiveDead%3 == 1 {
						// первый уровень: просто Enter (пин очереди)
						wake(*session)
						stats.record(Event{Kind: EvWake})
						ring.Add(Event{Time: now, Kind: EvWake, Note: "лёгкий пин (Enter)"})
						buf.Add("nvwatch: писательница обрыв >60с — лёгкий пин")
					} else if consecutiveDead%3 == 2 {
						// второй уровень: пин + задание
						wakeWithPrompt(*session, "Стрим оборвался — продолжай РЕЖИМ ЛУПА с места обрыва: прочитай _ПЛАН.md текущей книги и пиши следующую часть. Работай непрерывно.")
						stats.record(Event{Kind: EvWake})
						ring.Add(Event{Time: now, Kind: EvWake, Note: "будильник с заданием"})
						buf.Add("nvwatch: повторный обрыв — будильник с заданием в очередь")
					} else {
						// третий уровень: /new + полный промпт
						exec.Command("tmux", "send-keys", "-t", "nevis_zcode:"+*session, "-l", "/new").Run()
						time.Sleep(300 * time.Millisecond)
						exec.Command("tmux", "send-keys", "-t", "nevis_zcode:"+*session, "Enter").Run()
						time.Sleep(2 * time.Second)
						wakeWithPrompt(*session, "Ты — Невис-писатель в РЕЖИМЕ ЛУПА (AGENTS.md в этой папке). Свежая сессия: прочитай _ПЛАН.md текущей книги (папки по порядку) и продолжай со следующей части. Работай непрерывно. Буфер обрывов: /srv/run/nvwatch_buffer.txt — прочитай его, если там есть заметки, использованное сотри.")
						stats.record(Event{Kind: EvWake})
						ring.Add(Event{Time: now, Kind: EvWake, Note: "полный ребут (/new+наказ)"})
						buf.Add("nvwatch: глубокий обрыв — /new + полный промпт-наказ")
					}
					deadSince = now // сбрасываем таймер
				}
			} else {
				if !deadSince.IsZero() {
					buf.Add("nvwatch: писательница ожила после обрыва")
				}
				deadSince = time.Time{}
				consecutiveDead = 0
			}

			// --- сердце (в журнал, раз в 5 мин) ---
			if now.Sub(lastHeartT) >= 5*time.Minute {
				lastHeartT = now
				rss := zcodeRSS()
				stats.record(Event{Kind: EvHeart})
				ring.Add(Event{Time: now, Kind: EvHeart, Note: fmt.Sprintf("RSS %.1fГБ", rss)})
			}

			// --- TUI ---
			if now.Sub(lastUIT) >= time.Duration(*uiEvery)*time.Second {
				lastUIT = now
				uiMu.Lock()
				lp := lastProbe
				uiMu.Unlock()
				rss := zcodeRSS()
				bufN := 0
				if data, err := os.ReadFile(*bufferPath); err == nil {
					bufN = strings.Count(string(data), "\n")
				}
				render(stats, ring, rss, lp, gas, bufN)
			}
		}
	}
}
