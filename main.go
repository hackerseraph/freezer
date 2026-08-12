package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/getlantern/systray"
	"github.com/jlaffaye/ftp"
)

const retentionDays = 60
const metadataDirName = ".coldstorage"

var appConfig Config
var settingsMode bool

// Config holds runtime settings for the tray app.
type Config struct {
	Root          string
	Host          string
	User          string
	Pass          string
	FTPRoot       string
	SyncOnBattery bool
	Weekdays      [7]bool
	SyncHour      int
	SyncMinute    int
	Interval      time.Duration
}

// Record tracks a file's lifecycle in local cold storage.
type Record struct {
	LocalPath    string    `json:"local_path"`
	RemotePath   string    `json:"remote_path"`
	UploadedAt   time.Time `json:"uploaded_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Placeholder  bool      `json:"placeholder"`
	ContentHash  string    `json:"content_hash,omitempty"`
	LastModified time.Time `json:"last_modified"`
}

// IsExpired returns true when the file should be removed from local disk.
func (r Record) IsExpired(now time.Time) bool {
	return now.After(r.ExpiresAt) || now.Equal(r.ExpiresAt)
}

// State stores metadata for all synced files.
type State struct {
	Records map[string]Record `json:"records"`
}

func newState() *State {
	return &State{Records: make(map[string]Record)}
}

func defaultConfig() Config {
	return Config{
		Root:          ".",
		Host:          "",
		User:          "",
		Pass:          "",
		FTPRoot:       "/",
		SyncOnBattery: false,
		Weekdays:      [7]bool{},
		SyncHour:      23,
		SyncMinute:    0,
		Interval:      5 * time.Minute,
	}
}

func configFilePath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = "."
	}
	return filepath.Join(configDir, "coldstore", "settings.json")
}

func readPersistedConfig() (Config, error) {
	return readConfigFromFile(configFilePath())
}

func readConfigFromFile(cfgPath string) (Config, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	return saveConfigToFile(cfg, configFilePath())
}

func saveConfigToFile(cfg Config, cfgPath string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0600)
}

func loadConfig() Config {
	cfg := defaultConfig()
	if persisted, err := readPersistedConfig(); err == nil {
		cfg = persisted
	}

	flag.BoolVar(&settingsMode, "settings", false, "open the settings window")
	flag.StringVar(&cfg.Root, "root", cfg.Root, "Local folder to sync")
	flag.StringVar(&cfg.Host, "host", cfg.Host, "FTP host")
	flag.StringVar(&cfg.User, "user", cfg.User, "FTP username")
	flag.StringVar(&cfg.Pass, "pass", cfg.Pass, "FTP password")
	flag.StringVar(&cfg.FTPRoot, "ftp-root", cfg.FTPRoot, "FTP root folder")
	flag.BoolVar(&cfg.SyncOnBattery, "sync-on-battery", cfg.SyncOnBattery, "Allow sync while on battery")
	flag.DurationVar(&cfg.Interval, "interval", cfg.Interval, "Sync interval")
	flag.Parse()

	if v := os.Getenv("COLDSTORE_ROOT"); v != "" {
		cfg.Root = v
	}
	if v := os.Getenv("COLDSTORE_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("COLDSTORE_USER"); v != "" {
		cfg.User = v
	}
	if v := os.Getenv("COLDSTORE_PASS"); v != "" {
		cfg.Pass = v
	}
	if v := os.Getenv("COLDSTORE_FTP_ROOT"); v != "" {
		cfg.FTPRoot = v
	}
	if v := os.Getenv("COLDSTORE_SYNC_ON_BATTERY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.SyncOnBattery = b
		}
	}
	if v := os.Getenv("COLDSTORE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Interval = d
		}
	}
	if len(flag.Args()) > 0 && !strings.HasPrefix(flag.Args()[0], "-") {
		cfg.Root = flag.Args()[0]
	}
	if cfg.Root == "" {
		cfg.Root = defaultConfig().Root
	}
	return cfg
}

func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (s *State) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.Records = make(map[string]Record)
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.Records = make(map[string]Record)
		return nil
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}
	if loaded.Records == nil {
		loaded.Records = make(map[string]Record)
	}
	s.Records = loaded.Records
	return nil
}

func connectFTP(host, user, pass string) (*ftp.ServerConn, error) {
	conn, err := ftp.Dial(host)
	if err != nil {
		return nil, err
	}
	if err := conn.Login(user, pass); err != nil {
		_ = conn.Quit()
		return nil, err
	}
	return conn, nil
}

func remotePathFromLocal(rel, ftpRoot string) string {
	clean := filepath.ToSlash(filepath.Clean(rel))
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.ReplaceAll(clean, "\\", "/")
	base := strings.TrimSpace(ftpRoot)
	if base == "" {
		base = "/"
	}
	base = strings.ReplaceAll(base, "\\", "/")
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "/"
	}
	if clean == "" {
		return base
	}
	if base == "/" {
		return "/" + strings.TrimPrefix(clean, "/")
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(clean, "/")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func uploadFile(client *ftp.ServerConn, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	remoteDir := filepath.ToSlash(filepath.Dir(remotePath))
	if remoteDir != "." && remoteDir != "/" && remoteDir != "" {
		if err := ensureRemoteDir(client, remoteDir); err != nil {
			return err
		}
	}
	return client.Stor(remotePath, f)
}

func ensureRemoteDir(client *ftp.ServerConn, dir string) error {
	if dir == "." || dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	current := "/"
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = strings.TrimRight(current, "/") + "/" + part
		if err := client.MakeDir(current); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				return err
			}
		}
	}
	return nil
}

func createPlaceholder(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return nil
}

func archiveLocalFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return createPlaceholder(path)
}

func restoreFile(client *ftp.ServerConn, localPath string, rec Record) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	resp, err := client.Retr(rec.RemotePath)
	if err != nil {
		return err
	}
	defer resp.Close()

	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp); err != nil {
		return err
	}
	return nil
}

func syncFolder(root string, client *ftp.ServerConn, state *State) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if filepath.Base(path) == metadataDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == metadataDirName {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		remotePath := remotePathFromLocal(rel, appConfig.FTPRoot)

		record, ok := state.Records[path]
		now := time.Now()
		if !ok || record.Placeholder || record.LastModified.Before(info.ModTime()) || now.After(record.ExpiresAt) {
			contentHash, err := hashFile(path)
			if err != nil {
				return err
			}
			if err := uploadFile(client, path, remotePath); err != nil {
				return err
			}
			state.Records[path] = Record{
				LocalPath:    path,
				RemotePath:   remotePath,
				UploadedAt:   now,
				ExpiresAt:    now.Add(retentionDays * 24 * time.Hour),
				Placeholder:  false,
				ContentHash:  contentHash,
				LastModified: info.ModTime(),
			}
		}
		return nil
	})
}

func pruneExpired(client *ftp.ServerConn, state *State) error {
	now := time.Now()
	for localPath, record := range state.Records {
		if record.Placeholder {
			continue
		}
		if record.IsExpired(now) {
			if _, err := client.FileSize(record.RemotePath); err != nil {
				log.Printf("skip prune for %s: remote file missing - %v", localPath, err)
				continue
			}
			if err := archiveLocalFile(localPath); err != nil {
				return err
			}
			record.Placeholder = true
			record.ExpiresAt = now
			state.Records[localPath] = record
		}
	}
	return nil
}

func restorePlaceholderIfNeeded(path string, client *ftp.ServerConn, state *State) error {
	record, ok := state.Records[path]
	if !ok {
		return nil
	}
	if !record.Placeholder {
		return nil
	}
	if err := restoreFile(client, path, record); err != nil {
		return err
	}
	record.Placeholder = false
	record.UploadedAt = time.Now()
	record.ExpiresAt = time.Now().Add(retentionDays * 24 * time.Hour)
	state.Records[path] = record
	return nil
}

func runColdStorage(root, host, user, pass string) error {
	metadataPath := filepath.Join(root, metadataDirName, "index.json")
	state := newState()
	if err := state.Load(metadataPath); err != nil {
		return err
	}

	client, err := connectFTP(host, user, pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	if err := syncFolder(root, client, state); err != nil {
		return err
	}
	if err := pruneExpired(client, state); err != nil {
		return err
	}
	if err := state.Save(metadataPath); err != nil {
		return err
	}
	return nil
}

func restorePlaceholderFiles(root, host, user, pass string) error {
	metadataPath := filepath.Join(root, metadataDirName, "index.json")
	state := newState()
	if err := state.Load(metadataPath); err != nil {
		return err
	}

	client, err := connectFTP(host, user, pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	for localPath, record := range state.Records {
		if !record.Placeholder {
			continue
		}
		if err := restoreFile(client, localPath, record); err != nil {
			return err
		}
		record.Placeholder = false
		record.UploadedAt = time.Now()
		record.ExpiresAt = time.Now().Add(retentionDays * 24 * time.Hour)
		state.Records[localPath] = record
	}

	return state.Save(metadataPath)
}

func openFolder(path string) {
	if err := exec.Command("xdg-open", path).Start(); err != nil {
		log.Printf("cannot open folder %s: %v", path, err)
	}
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, col color.RGBA) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx, sy := 1, 1
	if x0 >= x1 {
		sx = -1
	}
	if y0 >= y1 {
		sy = -1
	}
	err := dx - dy
	for {
		if x0 >= 0 && x0 < img.Bounds().Dx() && y0 >= 0 && y0 < img.Bounds().Dy() {
			img.Set(x0, y0, col)
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func makeTrayIconData() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	bg := color.RGBA{R: 25, G: 118, B: 210, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)

	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	cx, cy := 32, 32
	lineLen := 18
	for _, d := range []struct{ x1, y1, x2, y2 int }{
		{cx, cy - lineLen, cx, cy + lineLen},
		{cx - lineLen, cy, cx + lineLen, cy},
		{cx - lineLen, cy - lineLen, cx + lineLen, cy + lineLen},
		{cx - lineLen, cy + lineLen, cx + lineLen, cy - lineLen},
		{cx - 12, cy - 17, cx + 12, cy + 17},
		{cx + 12, cy - 17, cx - 12, cy + 17},
	} {
		drawLine(img, d.x1, d.y1, d.x2, d.y2, white)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Printf("failed to encode tray icon: %v", err)
		return nil
	}
	return buf.Bytes()
}

func setTrayIcon() {
	icon := makeTrayIconData()
	if len(icon) == 0 {
		systray.SetTitle("")
		systray.SetTooltip("Freezer sync")
		return
	}
	systray.SetIcon(icon)
	systray.SetTemplateIcon(icon, icon)
	systray.SetTitle("")
	systray.SetTooltip("Freezer sync")
}

type compactTheme struct {
	fyne.Theme
}

func (t compactTheme) TextSize() float32           { return 12 }
func (t compactTheme) TextSubHeadingSize() float32 { return 14 }
func (t compactTheme) TextHeadingSize() float32    { return 18 }
func (t compactTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNameText {
		return 12
	}
	return t.Theme.Size(name)
}

func launchSettingsProcess() error {
	cmd := exec.Command(os.Args[0], "--settings")
	cmd.Env = os.Environ()
	return cmd.Start()
}

func openSettingsWindow() {
	if err := launchSettingsProcess(); err != nil {
		log.Printf("failed to launch settings window: %v", err)
	}
}

func refreshConfig() {
	cfg, err := readPersistedConfig()
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to reload config: %v", err)
		}
		return
	}
	appConfig = cfg
}

func chooseLocalFolder(parent fyne.Window, currentPath string, onSelect func(string)) {
	a := fyne.CurrentApp()
	if a == nil {
		return
	}

	picker := a.NewWindow("Select local folder")
	picker.Resize(fyne.NewSize(760, 500))
	picker.SetFixedSize(false)
	picker.SetCloseIntercept(func() {
		picker.Close()
	})

	pathEntry := widget.NewEntry()
	pathEntry.SetText(currentPath)
	pathEntry.SetPlaceHolder("Type or paste a folder path")
	rootBox := container.NewVBox()
	var currentFolder string
	var renderFolderList func(string)

	renderFolderList = func(folder string) {
		currentFolder = folder
		if currentFolder == "" {
			currentFolder = "."
		}
		absFolder, err := filepath.Abs(currentFolder)
		if err == nil {
			currentFolder = absFolder
		}
		pathEntry.SetText(currentFolder)

		items := []fyne.CanvasObject{}
		if parentDir := filepath.Dir(currentFolder); parentDir != currentFolder {
			items = append(items, widget.NewButton(".. (parent folder)", func() {
				renderFolderList(parentDir)
			}))
		}
		entries, err := os.ReadDir(currentFolder)
		if err != nil {
			items = append(items, widget.NewLabel("Unable to read folder"))
		} else {
			var dirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry.Name())
				}
			}
			sort.Strings(dirs)
			for _, name := range dirs {
				if name == metadataDirName {
					continue
				}
				fullPath := filepath.Join(currentFolder, name)
				items = append(items, widget.NewButton(name, func(path string) func() {
					return func() { renderFolderList(path) }
				}(fullPath)))
			}
			if len(dirs) == 0 {
				items = append(items, widget.NewLabel("No subfolders"))
			}
		}
		rootBox.Objects = items
		rootBox.Refresh()
	}

	if currentPath == "" {
		currentPath = "."
	}
	renderFolderList(currentPath)

	openHome := widget.NewButton("Home", func() {
		if home, err := os.UserHomeDir(); err == nil {
			renderFolderList(home)
		}
	})
	selectButton := widget.NewButton("Use this folder", func() {
		picked := strings.TrimSpace(pathEntry.Text)
		if picked == "" {
			picked = currentFolder
		}
		onSelect(picked)
		picker.Close()
	})
	cancelButton := widget.NewButton("Cancel", func() {
		picker.Close()
	})
	pathEntry.OnSubmitted = func(text string) {
		if text == "" {
			return
		}
		renderFolderList(text)
	}

	pathLabel := widget.NewLabelWithStyle("Location", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	pathField := container.NewBorder(nil, nil, nil, nil, pathEntry)
	buttonRow := container.NewHBox(
		layout.NewSpacer(),
		openHome,
		selectButton,
		cancelButton,
		layout.NewSpacer(),
	)
	topBar := container.NewVBox(
		container.NewPadded(pathLabel),
		container.NewPadded(pathField),
		container.NewPadded(buttonRow),
	)
	content := container.NewBorder(
		topBar,
		nil,
		nil,
		nil,
		container.NewPadded(container.NewScroll(rootBox)),
	)
	picker.SetContent(content)
	picker.Show()
}

func runSettingsWindow() {
	a := app.NewWithID("com.freezer.settings")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("Freezer Settings")
	w.Resize(fyne.NewSize(820, 470))
	w.SetFixedSize(false)
	w.SetCloseIntercept(func() {
		w.Close()
		a.Quit()
	})

	rootEntry := widget.NewEntry()
	rootEntry.SetText(appConfig.Root)
	rootEntry.SetPlaceHolder("Choose a local sync folder")
	hostEntry := widget.NewEntry()
	hostEntry.SetText(appConfig.Host)
	hostEntry.SetPlaceHolder("ftp.example.com:21")
	userEntry := widget.NewEntry()
	userEntry.SetText(appConfig.User)
	userEntry.SetPlaceHolder("username")
	passwordEntry := widget.NewPasswordEntry()
	passwordEntry.SetText(appConfig.Pass)
	passwordEntry.SetPlaceHolder("Password")
	ftpRootEntry := widget.NewEntry()
	ftpRootEntry.SetText(appConfig.FTPRoot)
	ftpRootEntry.SetPlaceHolder("/")
	batteryCheck := widget.NewCheck("Sync on battery", func(checked bool) {})
	batteryCheck.Checked = appConfig.SyncOnBattery
	batteryCheck.SetChecked(appConfig.SyncOnBattery)

	browseButton := widget.NewButton("Browse", func() {
		chooseLocalFolder(w, rootEntry.Text, func(path string) {
			rootEntry.SetText(path)
		})
	})
	browseButton.Importance = widget.LowImportance

	powerPanel := container.NewVBox(
		widget.NewLabelWithStyle("Power", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Machine-level power behavior."),
		container.NewPadded(batteryCheck),
	)

	weekdayLabels := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	weekdayChecks := make([]*widget.Check, len(weekdayLabels))
	for i, label := range weekdayLabels {
		weekdayChecks[i] = widget.NewCheck(label, func(checked bool) {})
		weekdayChecks[i].Checked = appConfig.Weekdays[i]
		weekdayChecks[i].SetChecked(appConfig.Weekdays[i])
	}
	throughputStatus := widget.NewLabel("No throughput test run yet.")
	throughputButton := widget.NewButton("Test throughput", func() {
		if appConfig.Host == "" || strings.Contains(appConfig.Host, "example.com") {
			dialog.NewError(fmt.Errorf("enter a real FTP server before testing throughput"), w).Show()
			return
		}
		if appConfig.User == "" {
			dialog.NewError(fmt.Errorf("FTP username is required"), w).Show()
			return
		}
		throughputStatus.SetText("Testing FTP throughput...")
		go func() {
			uploadMbps, downloadMbps, err := measureFTPThroughput(appConfig.Host, appConfig.User, appConfig.Pass, appConfig.FTPRoot)
			if err != nil {
				throughputStatus.SetText(fmt.Sprintf("Throughput test failed: %v", err))
				return
			}
			throughputStatus.SetText(fmt.Sprintf("Upload: %.2f Mb/s  |  Download: %.2f Mb/s", uploadMbps, downloadMbps))
		}()
	})
	schedulePanel := container.NewVBox(
		widget.NewLabelWithStyle("Sync Schedule", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Syncs start nightly at 11:00 PM."),
		widget.NewLabel("Initial sync must be kicked off manually from the tray menu."),
		container.NewGridWithColumns(7,
			weekdayChecks[0],
			weekdayChecks[1],
			weekdayChecks[2],
			weekdayChecks[3],
			weekdayChecks[4],
			weekdayChecks[5],
			weekdayChecks[6],
		),
		container.NewHBox(throughputButton),
		container.NewPadded(throughputStatus),
	)

	ftpPanel := container.NewVBox(
		widget.NewLabelWithStyle("FTP & Folder Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, browseButton, rootEntry),
		widget.NewLabel("FTP server"),
		hostEntry,
		widget.NewLabel("Username"),
		userEntry,
		widget.NewLabel("Password"),
		passwordEntry,
		widget.NewLabel("FTP root folder"),
		ftpRootEntry,
	)

	sections := map[string]fyne.CanvasObject{
		"Power":         powerPanel,
		"Sync Schedule": schedulePanel,
		"FTP & Folder":  ftpPanel,
	}
	order := []string{"Power", "Sync Schedule", "FTP & Folder"}
	selectedSection := "Power"
	rightPanel := container.NewVBox(sections[selectedSection])

	sidebar := container.NewVBox(
		widget.NewLabelWithStyle("Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	sidebarButtons := make(map[string]*widget.Button)
	for _, name := range order {
		btn := widget.NewButton(name, nil)
		if name == selectedSection {
			btn.Importance = widget.HighImportance
		}
		btn.OnTapped = func() {
			for _, s := range order {
				if b, ok := sidebarButtons[s]; ok {
					b.Importance = widget.LowImportance
				}
			}
			btn.Importance = widget.HighImportance
			selectedSection = name
			rightPanel.Objects = []fyne.CanvasObject{sections[name]}
			rightPanel.Refresh()
		}
		sidebarButtons[name] = btn
		sidebar.Add(btn)
	}
	saveButton := widget.NewButton("Save", func() {
		cfg := appConfig
		cfg.Root = strings.TrimSpace(rootEntry.Text)
		cfg.Host = strings.TrimSpace(hostEntry.Text)
		cfg.User = strings.TrimSpace(userEntry.Text)
		cfg.Pass = strings.TrimSpace(passwordEntry.Text)
		cfg.FTPRoot = strings.TrimSpace(ftpRootEntry.Text)
		cfg.SyncOnBattery = batteryCheck.Checked
		cfg.SyncHour = 23
		cfg.SyncMinute = 0
		for i, chk := range weekdayChecks {
			cfg.Weekdays[i] = chk.Checked
		}
		if cfg.FTPRoot == "" {
			cfg.FTPRoot = "/"
		}
		if cfg.Root == "" || cfg.Host == "" || strings.Contains(cfg.Host, "example.com") {
			dialog.NewError(fmt.Errorf("local folder and a real FTP server are required"), w).Show()
			return
		}
		if cfg.User == "" {
			dialog.NewError(fmt.Errorf("FTP username is required"), w).Show()
			return
		}
		if err := saveConfig(cfg); err != nil {
			dialog.NewError(err, w).Show()
			return
		}
		if err := os.MkdirAll(cfg.Root, 0755); err != nil {
			log.Printf("cannot create root folder %s: %v", cfg.Root, err)
		}
		appConfig = cfg
		w.Close()
		a.Quit()
	})
	cancelButton := widget.NewButton("Cancel", func() {
		w.Close()
		a.Quit()
	})
	bottomRow := container.NewHBox(
		layout.NewSpacer(),
		saveButton,
		cancelButton,
	)
	content := container.NewBorder(
		nil,
		container.NewPadded(bottomRow),
		sidebar,
		nil,
		container.NewPadded(rightPanel),
	)

	w.SetContent(content)
	w.Show()
	a.Run()
}

func onBatteryPower() bool {
	if _, err := exec.LookPath("upower"); err == nil {
		out, err := exec.Command("upower", "-e").Output()
		if err == nil && len(out) > 0 {
			if r, err := exec.Command("upower", "-i", "/org/freedesktop/UPower/devices/battery_BAT0").CombinedOutput(); err == nil {
				return strings.Contains(string(r), "state:         discharging") || strings.Contains(string(r), "state:         charging")
			}
		}
	}
	return false
}

func shouldSyncNow() bool {
	refreshConfig()
	if appConfig.SyncOnBattery {
		return true
	}
	return !onBatteryPower()
}

func isScheduledSyncWindow(now time.Time) bool {
	if appConfig.SyncHour < 0 || appConfig.SyncHour > 23 {
		appConfig.SyncHour = 23
	}
	if appConfig.SyncMinute < 0 || appConfig.SyncMinute > 59 {
		appConfig.SyncMinute = 0
	}
	if now.Hour() != appConfig.SyncHour || now.Minute() != appConfig.SyncMinute {
		return false
	}
	weekdayIndex := int(now.Weekday()) - 1
	if weekdayIndex < 0 {
		weekdayIndex = 6
	}
	return appConfig.Weekdays[weekdayIndex]
}

func measureFTPThroughput(host, user, pass, ftpRoot string) (uploadMbps float64, downloadMbps float64, err error) {
	client, err := connectFTP(host, user, pass)
	if err != nil {
		return 0, 0, err
	}
	defer client.Quit()

	testName := ".freezer-throughput-test.bin"
	remotePath := remotePathFromLocal(testName, ftpRoot)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 32768)
	payloadSize := len(payload)
	if payloadSize == 0 {
		return 0, 0, fmt.Errorf("empty payload")
	}

	uploadStart := time.Now()
	if err := client.Stor(remotePath, bytes.NewReader(payload)); err != nil {
		return 0, 0, err
	}
	uploadElapsed := time.Since(uploadStart).Seconds()
	if uploadElapsed <= 0 {
		uploadElapsed = 0.001
	}
	uploadMbps = (float64(payloadSize) * 8 / 1000000) / uploadElapsed

	downloadStart := time.Now()
	resp, err := client.Retr(remotePath)
	if err != nil {
		return uploadMbps, 0, err
	}
	defer resp.Close()

	buf := make([]byte, 65536)
	readBytes := 0
	for {
		readN, readErr := resp.Read(buf)
		if readN > 0 {
			readBytes += readN
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return uploadMbps, 0, readErr
		}
	}
	downloadElapsed := time.Since(downloadStart).Seconds()
	if downloadElapsed <= 0 {
		downloadElapsed = 0.001
	}
	downloadMbps = (float64(readBytes) * 8 / 1000000) / downloadElapsed
	_ = client.Delete(remotePath)
	return uploadMbps, downloadMbps, nil
}

func onReady() {
	setTrayIcon()
	systray.SetTitle("❄️")
	systray.SetTooltip("Freezer sync")

	statusItem := systray.AddMenuItem("Local: "+appConfig.Root, "Current local sync folder")
	statusItem.Disable()
	remoteStatusItem := systray.AddMenuItem("FTP root: "+appConfig.FTPRoot, "Current FTP root folder")
	remoteStatusItem.Disable()
	batteryStatusItem := systray.AddMenuItem("Sync on battery: "+strconv.FormatBool(appConfig.SyncOnBattery), "Whether background sync is allowed while on battery")
	batteryStatusItem.Disable()
	systray.AddSeparator()
	settingsItem := systray.AddMenuItem("Settings...", "Edit FTP and local folder configuration")
	syncItem := systray.AddMenuItem("Sync now", "Run an immediate sync")
	restoreItem := systray.AddMenuItem("Restore placeholders", "Download archived files back to disk")
	openFolderItem := systray.AddMenuItem("Open archive folder", "Open the local cold-storage folder")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit", "Exit the app")

	go func() {
		for {
			select {
			case <-settingsItem.ClickedCh:
				go openSettingsWindow()
			case <-syncItem.ClickedCh:
				refreshConfig()
				if !shouldSyncNow() {
					log.Printf("sync skipped: machine is on battery and sync-on-battery is disabled")
					continue
				}
				if err := runColdStorage(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
					log.Printf("sync failed: %v", err)
				}
			case <-restoreItem.ClickedCh:
				refreshConfig()
				if err := restorePlaceholderFiles(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
					log.Printf("restore failed: %v", err)
				}
			case <-openFolderItem.ClickedCh:
				openFolder(appConfig.Root)
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	go func() {
		for range time.Tick(time.Minute) {
			refreshConfig()
			if !shouldSyncNow() {
				continue
			}
			if !isScheduledSyncWindow(time.Now()) {
				continue
			}
			if err := runColdStorage(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
				log.Printf("scheduled sync failed: %v", err)
			}
		}
	}()
}

func onExit() {}

func main() {
	appConfig = loadConfig()
	if settingsMode {
		runSettingsWindow()
		return
	}
	fmt.Printf("Freezer tray app starting in %s\n", appConfig.Root)
	systray.Run(onReady, onExit)
}
