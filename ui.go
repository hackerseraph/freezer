package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
)

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

func makeTrayIconICO() []byte {
	pngData := makeTrayIconData()
	if len(pngData) == 0 {
		return nil
	}

	dataSize := uint32(len(pngData))
	dataOffset := uint32(6 + 16)

	var buf bytes.Buffer
	buf.Write([]byte{0, 0})
	buf.Write([]byte{1, 0})
	buf.Write([]byte{1, 0})

	buf.WriteByte(64)
	buf.WriteByte(64)
	buf.WriteByte(0)
	buf.WriteByte(0)
	buf.Write([]byte{1, 0})
	buf.Write([]byte{32, 0})
	buf.Write([]byte{
		byte(dataSize), byte(dataSize >> 8),
		byte(dataSize >> 16), byte(dataSize >> 24),
	})
	buf.Write([]byte{
		byte(dataOffset), byte(dataOffset >> 8),
		byte(dataOffset >> 16), byte(dataOffset >> 24),
	})

	buf.Write(pngData)
	return buf.Bytes()
}

func setTrayIcon() {
	systray.SetTooltip("Freezer sync")
	systray.SetTitle("")

	if runtime.GOOS == "windows" {
		return
	}

	iconData := makeTrayIconData()
	if len(iconData) == 0 {
		return
	}
	systray.SetIcon(iconData)
	systray.SetTemplateIcon(iconData, iconData)
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

func launchAboutProcess() error {
	cmd := exec.Command(os.Args[0], "--about")
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

func getCommonFolders() map[string]string {
	folders := make(map[string]string)

	if home, err := os.UserHomeDir(); err == nil {
		folders["Home"] = home
	}
	if desktop, err := os.UserHomeDir(); err == nil {
		folders["Desktop"] = filepath.Join(desktop, "Desktop")
	}
	if docs, err := os.UserHomeDir(); err == nil {
		folders["Documents"] = filepath.Join(docs, "Documents")
	}
	if downloads, err := os.UserHomeDir(); err == nil {
		folders["Downloads"] = filepath.Join(downloads, "Downloads")
	}

	return folders
}

func chooseLocalFolder(parent fyne.Window, currentPath string, onSelect func(string)) {
	a := fyne.CurrentApp()
	if a == nil {
		return
	}

	picker := a.NewWindow("Select Folder")
	picker.Resize(fyne.NewSize(900, 600))
	picker.SetFixedSize(false)
	picker.SetCloseIntercept(func() {
		picker.Close()
	})

	var currentFolder string
	if currentPath == "" {
		currentPath = "."
	}
	absFolder, err := filepath.Abs(currentPath)
	if err == nil {
		currentFolder = absFolder
	} else {
		currentFolder = currentPath
	}

	history := []string{currentFolder}
	historyIndex := 0

	folderDisplay := container.NewVBox()
	scrollContainer := container.NewScroll(folderDisplay)
	pathEntry := widget.NewEntry()
	pathEntry.SetPlaceHolder("Enter folder path...")

	var refreshFolder func(string)
	var updatePathDisplay func()

	breadcrumbContainer := container.NewHBox()
	updateBreadcrumb := func() {
		breadcrumbContainer.Objects = []fyne.CanvasObject{}
		parts := strings.Split(strings.TrimPrefix(currentFolder, "/"), "/")
		if currentFolder == "/" {
			parts = []string{""}
		}

		path := ""
		for i, part := range parts {
			if part == "" && i == 0 {
				p := "/"
				btn := widget.NewButton(p, func(targetPath string) func() {
					return func() { refreshFolder(targetPath) }
				}("/"))
				btn.Importance = widget.LowImportance
				breadcrumbContainer.Add(btn)
			} else if part != "" {
				path = filepath.Join(path, part)
				btnLabel := part
				if i < len(parts)-1 {
					btnLabel = part + " >"
				}
				btn := widget.NewButton(btnLabel, func(targetPath string) func() {
					return func() { refreshFolder(targetPath) }
				}(path))
				btn.Importance = widget.LowImportance
				breadcrumbContainer.Add(btn)
			}
		}
	}

	updatePathDisplay = func() {
		pathEntry.SetText(currentFolder)
		updateBreadcrumb()
		breadcrumbContainer.Refresh()
	}

	refreshFolder = func(folder string) {
		currentFolder = folder
		if currentFolder == "" {
			currentFolder = "."
		}
		absFolder, err := filepath.Abs(currentFolder)
		if err == nil {
			currentFolder = absFolder
		}

		if historyIndex < len(history)-1 {
			history = history[:historyIndex+1]
		}
		if len(history) == 0 || history[len(history)-1] != currentFolder {
			history = append(history, currentFolder)
			historyIndex = len(history) - 1
		}

		updatePathDisplay()

		folderDisplay.Objects = []fyne.CanvasObject{}
		entries, err := os.ReadDir(currentFolder)
		if err != nil {
			folderDisplay.Add(widget.NewLabel("Unable to read folder"))
		} else {
			var dirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry.Name())
				}
			}
			sort.Strings(dirs)

			if len(dirs) == 0 {
				folderDisplay.Add(widget.NewLabel("No subfolders"))
			} else {
				for _, name := range dirs {
					if name == metadataDirName {
						continue
					}
					if strings.HasPrefix(name, ".") {
						continue
					}
					fullPath := filepath.Join(currentFolder, name)
					folderName := name
					btn := widget.NewButton("📁 "+folderName, func(path string) func() {
						return func() { refreshFolder(path) }
					}(fullPath))
					folderDisplay.Add(btn)
				}
			}
		}
		folderDisplay.Refresh()
		scrollContainer.Refresh()
	}

	sidebar := container.NewVBox(
		widget.NewLabelWithStyle("Quick Access", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	commonFolders := getCommonFolders()
	for name, path := range map[string]string{"Home": commonFolders["Home"], "Desktop": commonFolders["Desktop"], "Documents": commonFolders["Documents"], "Downloads": commonFolders["Downloads"]} {
		if path != "" {
			btn := widget.NewButton(name, func(p string) func() {
				return func() { refreshFolder(p) }
			}(path))
			btn.Importance = widget.LowImportance
			sidebar.Add(btn)
		}
	}

	backBtn := widget.NewButton("← Back", func() {
		if historyIndex > 0 {
			historyIndex--
			currentFolder = history[historyIndex]
			updatePathDisplay()
			folderDisplay.Objects = []fyne.CanvasObject{}
			entries, _ := os.ReadDir(currentFolder)
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
				btn := widget.NewButton("📁 "+name, func(path string) func() {
					return func() { refreshFolder(path) }
				}(fullPath))
				folderDisplay.Add(btn)
			}
			folderDisplay.Refresh()
			scrollContainer.Refresh()
		}
	})
	forwardBtn := widget.NewButton("Forward →", func() {
		if historyIndex < len(history)-1 {
			historyIndex++
			currentFolder = history[historyIndex]
			updatePathDisplay()
			folderDisplay.Objects = []fyne.CanvasObject{}
			entries, _ := os.ReadDir(currentFolder)
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
				btn := widget.NewButton("📁 "+name, func(path string) func() {
					return func() { refreshFolder(path) }
				}(fullPath))
				folderDisplay.Add(btn)
			}
			folderDisplay.Refresh()
			scrollContainer.Refresh()
		}
	})
	upBtn := widget.NewButton("⬆ Up", func() {
		parent := filepath.Dir(currentFolder)
		if parent != currentFolder {
			refreshFolder(parent)
		}
	})
	for _, b := range []*widget.Button{backBtn, forwardBtn, upBtn} {
		b.Importance = widget.LowImportance
	}

	toolbar := container.NewHBox(backBtn, forwardBtn, upBtn, layout.NewSpacer())

	pathEntry.OnSubmitted = func(text string) {
		if text != "" {
			refreshFolder(text)
		}
	}

	selectButton := widget.NewButton("Select Folder", func() {
		picked := strings.TrimSpace(pathEntry.Text)
		if picked == "" {
			picked = currentFolder
		}
		onSelect(picked)
		picker.Close()
	})
	selectButton.Importance = widget.HighImportance
	cancelButton := widget.NewButton("Cancel", func() {
		picker.Close()
	})
	bottomBar := container.NewHBox(
		layout.NewSpacer(),
		selectButton,
		cancelButton,
	)

	refreshFolder(currentFolder)

	mainContent := container.NewBorder(
		container.NewVBox(toolbar, pathEntry),
		bottomBar,
		sidebar,
		nil,
		scrollContainer,
	)

	picker.SetContent(mainContent)
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
	preventSleepCheck := widget.NewCheck("Prevent sleep during sync", func(checked bool) {})
	preventSleepCheck.Checked = appConfig.PreventSleepDuringSync
	preventSleepCheck.SetChecked(appConfig.PreventSleepDuringSync)

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
		container.NewPadded(preventSleepCheck),
	)

	weekdayLabels := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	weekdayChecks := make([]*widget.Check, len(weekdayLabels))
	for i, label := range weekdayLabels {
		weekdayChecks[i] = widget.NewCheck(label, func(checked bool) {})
		weekdayChecks[i].Checked = appConfig.Weekdays[i]
		weekdayChecks[i].SetChecked(appConfig.Weekdays[i])
	}
	throughputStatus := widget.NewLabel("No throughput test run yet.")
	throughputProgress := widget.NewProgressBarInfinite()
	throughputProgress.Hide()

	throughputContainer := container.NewVBox(
		throughputStatus,
		throughputProgress,
	)

	throughputButton := widget.NewButton("Test throughput", nil)
	throughputButton.OnTapped = func() {
		if appConfig.Host == "" || strings.Contains(appConfig.Host, "example.com") {
			dialog.NewError(fmt.Errorf("enter a real FTP server before testing throughput"), w).Show()
			return
		}
		if appConfig.User == "" {
			dialog.NewError(fmt.Errorf("FTP username is required"), w).Show()
			return
		}

		throughputButton.Disable()
		throughputStatus.SetText("Testing FTP throughput...")
		throughputProgress.Show()

		go func() {
			uploadMbps, downloadMbps, err := measureFTPThroughput(appConfig.Host, appConfig.User, appConfig.Pass, appConfig.FTPRoot)
			throughputProgress.Hide()
			if err != nil {
				throughputStatus.SetText(fmt.Sprintf("Throughput test failed: %v", err))
			} else {
				throughputStatus.SetText(fmt.Sprintf("Upload: %.2f Mb/s  |  Download: %.2f Mb/s", uploadMbps, downloadMbps))
			}
			throughputButton.Enable()
		}()
	}
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
		container.NewPadded(throughputContainer),
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
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Server Capabilities", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Probe the server to discover which integrity checks are available."),
	)

	capStatusLabel := widget.NewLabel("")
	verifySizeCheck := widget.NewCheck("Verify file size before archiving — SIZE (lowest level, confirms existence and completeness only)", func(bool) {})
	verifySizeCheck.SetChecked(appConfig.VerifyUploadSize)
	verifyHashCheck := widget.NewCheck("Verify file hash before archiving", func(bool) {})
	verifyHashCheck.SetChecked(appConfig.VerifyUploadHash)
	verifyHashCheck.Disable()
	allowUnsafeCheck := widget.NewCheck("Allow freezing even when no integrity verification is available (at your own risk)", func(bool) {})
	allowUnsafeCheck.SetChecked(appConfig.AllowUnsafeFreeze)

	probeBtn := widget.NewButton("Probe server capabilities", nil)
	probeBtn.OnTapped = func() {
		host := strings.TrimSpace(hostEntry.Text)
		user := strings.TrimSpace(userEntry.Text)
		pass := strings.TrimSpace(passwordEntry.Text)
		if host == "" || strings.Contains(host, "example.com") {
			capStatusLabel.SetText("Enter a real FTP server first.")
			return
		}
		probeBtn.Disable()
		capStatusLabel.SetText("Probing...")
		go func() {
			caps, err := probeFTPCapabilities(host, user, pass)
			if err != nil {
				capStatusLabel.SetText("Probe failed: " + err.Error())
				probeBtn.Enable()
				return
			}
			capStatusLabel.SetText(caps.Summary())
			verifySizeCheck.SetChecked(caps.SIZE)
			if caps.BestHashCommand() != "" {
				verifyHashCheck.SetChecked(true)
				verifyHashCheck.Enable()
				verifyHashCheck.Text = "Verify file hash before archiving (" + caps.BestHashCommand() + ")"
				verifyHashCheck.Refresh()
			} else {
				verifyHashCheck.SetChecked(false)
				verifyHashCheck.Disable()
				verifyHashCheck.Text = "Verify file hash before archiving (not supported by server)"
				verifyHashCheck.Refresh()
			}
			probeBtn.Enable()
		}()
	}

	capPanel := container.NewVBox(
		container.NewHBox(probeBtn),
		container.NewPadded(capStatusLabel),
		container.NewPadded(verifySizeCheck),
		container.NewPadded(verifyHashCheck),
		container.NewPadded(allowUnsafeCheck),
	)

	ftpPanelFull := container.NewVBox(
		ftpPanel,
		capPanel,
	)

	excludedSet := make(map[string]bool)
	for _, ex := range appConfig.ExcludedFolders {
		excludedSet[ex] = true
	}
	var excludeCheckboxes []*widget.Check
	excludeList := container.NewVBox()
	excludeScroll := container.NewScroll(excludeList)
	excludeScroll.SetMinSize(fyne.NewSize(0, 220))

	var refreshExcludeList func(root string)
	refreshExcludeList = func(root string) {
		excludeList.Objects = nil
		excludeCheckboxes = nil
		if root == "" {
			excludeList.Add(widget.NewLabel("Select a root folder in FTP & Folder first."))
			excludeList.Refresh()
			return
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			excludeList.Add(widget.NewLabel("Cannot read root folder."))
			excludeList.Refresh()
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == metadataDirName {
				continue
			}
			name := entry.Name()
			chk := widget.NewCheck(name, func(bool) {})
			chk.SetChecked(excludedSet[name])
			excludeCheckboxes = append(excludeCheckboxes, chk)
			excludeList.Add(chk)
		}
		if len(excludeCheckboxes) == 0 {
			excludeList.Add(widget.NewLabel("No subfolders found in root."))
		}
		excludeList.Refresh()
		excludeScroll.Refresh()
	}
	refreshExcludeList(appConfig.Root)

	rootEntry.OnChanged = func(text string) {
		refreshExcludeList(strings.TrimSpace(text))
	}

	exclusionsPanel := container.NewVBox(
		widget.NewLabelWithStyle("Excluded Folders", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Checked folders will be skipped during sync."),
		excludeScroll,
	)

	shellIntegrationStatus := widget.NewLabel("")
	installShellBtn := widget.NewButton("Install shell integration", func() {
		if err := installShellIntegration(); err != nil {
			shellIntegrationStatus.SetText("Failed: " + err.Error())
		} else {
			shellIntegrationStatus.SetText("Installed! Double-click any .frozen file to restore it.")
		}
	})
	systemPanel := container.NewVBox(
		widget.NewLabelWithStyle("System Integration", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Register .frozen files with the OS so double-clicking restores them."),
		container.NewHBox(installShellBtn),
		container.NewPadded(shellIntegrationStatus),
	)

	retentionDays := appConfig.RetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultRetentionDays
	}
	retentionLabel := widget.NewLabel(fmt.Sprintf("Keep files warm for %d days before freezing", retentionDays))
	retentionSlider := widget.NewSlider(7, 365)
	retentionSlider.Step = 1
	retentionSlider.Value = float64(retentionDays)
	retentionSlider.OnChanged = func(v float64) {
		retentionDays = int(v)
		retentionLabel.SetText(fmt.Sprintf("Keep files warm for %d days before freezing", retentionDays))
	}

	tickRow := container.NewHBox(
		widget.NewLabelWithStyle("7 days", fyne.TextAlignLeading, fyne.TextStyle{}),
		layout.NewSpacer(),
		widget.NewLabelWithStyle("365 days", fyne.TextAlignTrailing, fyne.TextStyle{}),
	)

	storagePanel := container.NewVBox(
		widget.NewLabelWithStyle("Storage Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Configure how long files stay on local disk before being archived to cold storage."),
		container.NewPadded(retentionLabel),
		retentionSlider,
		tickRow,
	)

	encryptCheck := widget.NewCheck("Encrypt files before uploading to FTP", func(bool) {})
	encryptCheck.SetChecked(appConfig.EncryptionEnabled)
	alreadyConfigured := appConfig.EncryptionSalt != ""
	passphraseEntry := widget.NewPasswordEntry()
	confirmEntry := widget.NewPasswordEntry()
	cryptoStatus := widget.NewLabel("")
	var passphraseLabel, confirmLabel *widget.Label

	if alreadyConfigured {
		passphraseEntry.SetPlaceHolder("Enter passphrase to unlock or change")
		confirmEntry.Hide()
		cryptoStatus.SetText("Encryption configured. Passphrase is not stored on disk.")
		passphraseLabel = widget.NewLabel("Enter passphrase to unlock for this session:")
		confirmLabel = widget.NewLabel("")
	} else {
		passphraseEntry.SetPlaceHolder("New passphrase")
		confirmEntry.SetPlaceHolder("Confirm passphrase")
		cryptoStatus.SetText("Passphrase is never stored on disk. You will be prompted on startup.")
		passphraseLabel = widget.NewLabel("Passphrase (Argon2id-derived AES-256 key, never stored on disk):")
		confirmLabel = widget.NewLabel("Confirm passphrase:")
	}

	unlockBtn := widget.NewButton("Unlock for this session", func() {
		p := strings.TrimSpace(passphraseEntry.Text)
		if p == "" {
			cryptoStatus.SetText("Passphrase cannot be empty.")
			return
		}
		key, err := deriveKey(p, appConfig.EncryptionSalt)
		if err != nil {
			cryptoStatus.SetText("Key derivation failed: " + err.Error())
			return
		}
		activeEncryptionKey = key
		cryptoStatus.SetText("Encryption unlocked for this session.")
		passphraseEntry.SetText("")
	})
	if !alreadyConfigured {
		unlockBtn.Hide()
	}

	genSaltBtn := widget.NewButton("Re-generate salt (invalidates existing archive)", func() {
		salt, err := newEncryptionSalt()
		if err != nil {
			cryptoStatus.SetText("Failed to generate salt: " + err.Error())
			return
		}
		appConfig.EncryptionSalt = salt
		activeEncryptionKey = nil
		cryptoStatus.SetText("New salt generated. Save and re-enter passphrase. All existing encrypted archives are no longer recoverable.")
	})
	genSaltBtn.Importance = widget.DangerImportance

	cryptoPanel := container.NewVBox(
		widget.NewLabelWithStyle("Cryptography", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("When enabled, file content is encrypted with AES-256-GCM before upload."),
		widget.NewLabel("The FTP server holds only ciphertext. Passphrase is never written to disk."),
		container.NewPadded(encryptCheck),
		passphraseLabel,
		passphraseEntry,
		confirmLabel,
		confirmEntry,
		container.NewHBox(unlockBtn),
		container.NewPadded(cryptoStatus),
		widget.NewSeparator(),
		widget.NewLabel("WARNING: if you lose your passphrase, encrypted archives cannot be recovered."),
		container.NewHBox(genSaltBtn),
	)

	sections := map[string]fyne.CanvasObject{
		"Power":         powerPanel,
		"Sync Schedule": schedulePanel,
		"FTP & Folder":  ftpPanelFull,
		"Exclusions":    exclusionsPanel,
		"Storage":       storagePanel,
		"Cryptography":  cryptoPanel,
		"System":        systemPanel,
	}
	order := []string{"Power", "Sync Schedule", "FTP & Folder", "Exclusions", "Storage", "Cryptography", "System"}
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
					b.Refresh()
				}
			}
			btn.Importance = widget.HighImportance
			btn.Refresh()
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
		cfg.PreventSleepDuringSync = preventSleepCheck.Checked
		cfg.RetentionDays = retentionDays
		cfg.VerifyUploadSize = verifySizeCheck.Checked
		cfg.VerifyUploadHash = verifyHashCheck.Checked
		cfg.AllowUnsafeFreeze = allowUnsafeCheck.Checked
		if verifyHashCheck.Checked {
			label := verifyHashCheck.Text
			if i := strings.LastIndex(label, "("); i >= 0 {
				cfg.HashCommand = strings.TrimSuffix(label[i+1:], ")")
			}
		}
		cfg.EncryptionEnabled = encryptCheck.Checked
		if encryptCheck.Checked && !alreadyConfigured {
			p := strings.TrimSpace(passphraseEntry.Text)
			c := strings.TrimSpace(confirmEntry.Text)
			if p == "" {
				dialog.NewError(fmt.Errorf("passphrase cannot be empty when encryption is enabled"), w).Show()
				return
			}
			if p != c {
				dialog.NewError(fmt.Errorf("passphrases do not match"), w).Show()
				return
			}
			salt, err := newEncryptionSalt()
			if err != nil {
				dialog.NewError(fmt.Errorf("failed to generate encryption salt: %v", err), w).Show()
				return
			}
			cfg.EncryptionSalt = salt
			key, err := deriveKey(p, salt)
			if err != nil {
				dialog.NewError(fmt.Errorf("key derivation failed: %v", err), w).Show()
				return
			}
			activeEncryptionKey = key
		} else {
			cfg.EncryptionSalt = appConfig.EncryptionSalt
		}
		var excluded []string
		for _, chk := range excludeCheckboxes {
			if chk.Checked {
				excluded = append(excluded, chk.Text)
			}
		}
		cfg.ExcludedFolders = excluded
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

func showAboutWindow() {
	a := app.NewWithID("com.freezer.about")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("About Freezer")
	w.Resize(fyne.NewSize(620, 560))
	w.SetFixedSize(false)
	w.SetCloseIntercept(func() { w.Close(); a.Quit() })

	disclaimer := `DISCLAIMER — NO WARRANTY — LIMITATION OF LIABILITY

Freezer is provided "as is", without warranty of any kind, express or
implied, including but not limited to the warranties of merchantability,
fitness for a particular purpose, and non-infringement.

In no event shall the authors or copyright holders be liable for any
claim, damages, or other liability — whether in an action of contract,
tort, or otherwise — arising from, out of, or in connection with the
software or the use or other dealings in the software.

THIS INCLUDES BUT IS NOT LIMITED TO: loss of data, loss of encrypted
archives due to forgotten passphrases, FTP server outages, file
corruption, accidental deletion, interrupted sync operations, or any
financial loss resulting from inability to access archived files.

YOU ARE SOLELY RESPONSIBLE FOR:
  - Maintaining a secure backup of your encryption passphrase
  - Verifying that files have been successfully archived before deletion
  - Ensuring your FTP server is reliable and has sufficient storage
  - Any data that cannot be recovered due to key loss or server failure

Use this software at your own risk.`

	privacy := `PRIVACY POLICY

Freezer collects no data about you, your files, or your usage.

  - No telemetry
  - No analytics
  - No crash reporting
  - No usage statistics
  - No network connections except to your own configured FTP server
  - No third-party services of any kind

All data processed by Freezer stays on your local machine and your own
FTP server. The developers have no access to your files, your settings,
your encryption keys, or any other information about your use of this
software.

The only outbound network activity this application performs is FTP
transfers to the server address you configure yourself.`

	closeBtn := widget.NewButton("Close", func() { w.Close(); a.Quit() })
	closeBtn.Importance = widget.HighImportance

	content := container.NewScroll(container.NewVBox(
		widget.NewLabelWithStyle("Freezer  v"+appVersion, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Client-side cold storage with FTP archiving and AES-256-GCM encryption."),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Disclaimer", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(disclaimer),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Privacy Policy", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(privacy),
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), closeBtn, layout.NewSpacer()),
	))

	w.SetContent(content)
	w.Show()
	a.Run()
}

func launchPassphrasePrompt() {
	a := app.NewWithID("com.freezer.unlock")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("Unlock Encryption")
	w.Resize(fyne.NewSize(400, 180))
	w.SetFixedSize(true)
	w.SetCloseIntercept(func() { w.Close(); a.Quit() })

	entry := widget.NewPasswordEntry()
	entry.SetPlaceHolder("Enter passphrase")
	status := widget.NewLabel("")

	okBtn := widget.NewButton("Unlock", func() {
		p := strings.TrimSpace(entry.Text)
		if p == "" {
			status.SetText("Passphrase cannot be empty.")
			return
		}
		key, err := deriveKey(p, appConfig.EncryptionSalt)
		if err != nil {
			status.SetText("Error: " + err.Error())
			return
		}
		activeEncryptionKey = key
		log.Printf("Encryption unlocked for this session")
		w.Close()
		a.Quit()
	})
	okBtn.Importance = widget.HighImportance

	cancelBtn := widget.NewButton("Cancel", func() { w.Close(); a.Quit() })

	w.SetContent(container.NewVBox(
		widget.NewLabel("Enter your encryption passphrase to enable encrypted sync for this session."),
		entry,
		status,
		container.NewHBox(layout.NewSpacer(), okBtn, cancelBtn),
	))
	w.Show()
	a.Run()
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
	sleepStatusItem := systray.AddMenuItem("Prevent sleep during sync: "+strconv.FormatBool(appConfig.PreventSleepDuringSync), "Whether to prevent machine sleep during sync")
	sleepStatusItem.Disable()
	systray.AddSeparator()
	settingsItem := systray.AddMenuItem("Settings...", "Edit FTP and local folder configuration")
	unlockItem := systray.AddMenuItem("Unlock encryption...", "Enter passphrase to unlock encrypted sync for this session")
	if !appConfig.EncryptionEnabled {
		unlockItem.Hide()
	}
	syncItem := systray.AddMenuItem("Sync now", "Run an immediate sync")
	restoreItem := systray.AddMenuItem("Restore placeholders", "Download archived files back to disk")
	openFolderItem := systray.AddMenuItem("Open archive folder", "Open the local cold-storage folder")
	systray.AddSeparator()
	aboutItem := systray.AddMenuItem("About Freezer", "Version info, privacy policy, and disclaimer")
	quitItem := systray.AddMenuItem("Quit", "Exit the app")

	go func() {
		for {
			select {
			case <-settingsItem.ClickedCh:
				go openSettingsWindow()
			case <-unlockItem.ClickedCh:
				go launchPassphrasePrompt()
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
			case <-aboutItem.ClickedCh:
				go func() {
					if err := launchAboutProcess(); err != nil {
						log.Printf("failed to launch about window: %v", err)
					}
				}()
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
