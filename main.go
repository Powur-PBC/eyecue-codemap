package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"github.com/akamensky/base58"
	"github.com/mattn/go-isatty"
	"hash"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type TokenLocation struct {
	filename   string
	lineNum    int
	linkToFile bool
}

type TokenGroupInfo struct {
	token           string
	fileSource      FileSource
	startLineNumber int
	endLineNumber   int
	actualHash      string
	expectedHash    string
}

type TokenMap struct {
	Single map[string][]TokenLocation
	Group  map[string][]TokenGroupInfo
}

var tokenNeededRegexp = regexp.MustCompile(`\[eyecue-codemap(-group)?]`)
var tokenRegexp = regexp.MustCompile(`^(.*)\[eyecue-codemap:([A-Za-z0-9]+)](.*)$`)
var tokenGroupStartRegexp = regexp.MustCompile(`\[eyecue-codemap-group:([A-Za-z0-9]+)]`)
var tokenGroupEndRegexp = regexp.MustCompile(`\[end-eyecue-codemap-group:([A-Za-z0-9]+)(:([a-f0-9]{40}))?]`)
var tokenRefRegexp = regexp.MustCompile(`<!--eyecue-codemap:[A-Za-z0-9]+-->]\(.*?\)`)

var ignoreExtensions = []string{
	".csv",
	".jpeg",
	".jpg",
	".otf",
	".png",
	".ttf",
	".webp",
	".woff",
	".woff2",
}

var Version string = "dev"

type FilenameSource int

const (
	FilenameSourceStdin FilenameSource = iota
	FilenameSourceStdinNul
	FilenameSourceGit
	FilenameSourceGitIndex
)

type Config struct {
	AckGroups      bool
	CheckOnly      bool
	FilenameSource FilenameSource
	NoUnused       bool
	Verbose        bool
}

type FileSource struct {
	Filename     string
	FromGitIndex bool
}

func main() {
	var config Config

	for _, arg := range os.Args[1:] {
		switch arg {
		case "--help", "-h":
			fmt.Printf("eyecue-codemap version %s\n"+
				"Usage: eyecue-codemap [--check-only] [--git-index] [--no-unused]\n"+
				"If not using --git-index, pipe in a list of filenames to stdin, one per line.\n", Version)
			os.Exit(0)
		case "ack":
			config.AckGroups = true
		case "--check-only":
			config.CheckOnly = true
		case "--git":
			config.FilenameSource = FilenameSourceGit
		case "--git-index":
			config.FilenameSource = FilenameSourceGitIndex
		case "--no-unused":
			config.NoUnused = true
		case "--stdin":
			config.FilenameSource = FilenameSourceStdin
		case "--stdin0":
			config.FilenameSource = FilenameSourceStdinNul
		case "--verbose":
			config.Verbose = true
		default:
			fmt.Printf("ERROR: unrecognized argument: %s\n", arg)
			os.Exit(2)
		}
	}

	if config.FilenameSource == FilenameSourceGitIndex && !config.CheckOnly {
		fmt.Println("ERROR: --check-only must be specified when using --git-index")
		os.Exit(2)
	}

	if config.AckGroups && config.CheckOnly {
		fmt.Println("ERROR: cannot specify both ack and --check-only")
		os.Exit(2)
	}

	err := run(config)
	if err != nil {
		fmt.Printf("eyecue-codemap error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("eyecue-codemap completed successfully")
}

func run(config Config) error {
	var modeDesc string

	switch config.FilenameSource {
	case FilenameSourceGit:
		modeDesc = "Git"
	case FilenameSourceGitIndex:
		modeDesc = "Git index"
	case FilenameSourceStdin:
		modeDesc = "stdin"
	case FilenameSourceStdinNul:
		modeDesc = "stdin, NUL delimited"
	}

	if config.CheckOnly {
		modeDesc += ", check only"
	}

	fmt.Printf("eyecue-codemap %s running (filenames from %s) ...\n", Version, modeDesc)

	var fileSources []FileSource
	var err error

	switch config.FilenameSource {
	case FilenameSourceGit:
		fileSources, err = readFilenamesFromGit()
	case FilenameSourceGitIndex:
		fileSources, err = readFilenamesFromGitIndex()
	case FilenameSourceStdin:
		fileSources, err = readFilenamesFromStdin(false)
	case FilenameSourceStdinNul:
		fileSources, err = readFilenamesFromStdin(true)
	}

	if err != nil {
		return err
	}

	sort.Slice(fileSources, func(i, j int) bool {
		return fileSources[i].Filename < fileSources[j].Filename
	})

	tokenMap := TokenMap{
		Single: map[string][]TokenLocation{},
		Group:  map[string][]TokenGroupInfo{},
	}
	var mdFileSources []FileSource

	// inventory each input file, generating tokens and updating them if needed
	for _, fileSource := range fileSources {
		err := processFile(config, fileSource, tokenMap)
		if err != nil {
			return err
		}

		if strings.ToLower(path.Ext(fileSource.Filename)) == ".md" {
			mdFileSources = append(mdFileSources, fileSource)
		}
	}

	unusedTokens := make(map[string]struct{})
	var dupTokensErrs []string

	for token, tokenLocs := range tokenMap.Single {
		if len(tokenLocs) > 1 {
			errMsg := fmt.Sprintf("duplicate token \"%s\" at:", token)
			for _, tokenLoc := range tokenLocs {
				errMsg = fmt.Sprintf("%s\n   %s:%d", errMsg, tokenLoc.filename, tokenLoc.lineNum)
			}
			dupTokensErrs = append(dupTokensErrs, errMsg)
		}

		unusedTokens[token] = struct{}{}
	}

	if len(dupTokensErrs) > 0 {
		return errors.New(strings.Join(dupTokensErrs, "\n"))
	}

	// check or update the Markdown files
	for _, fileSource := range mdFileSources {
		err := processMarkdownFile(config, fileSource, tokenMap, unusedTokens)
		if err != nil {
			return err
		}
	}

	// show unused tokens
	var unusedTokenErrs []string
	for token := range unusedTokens {
		tokenLoc := tokenMap.Single[token][0]
		msg := fmt.Sprintf("unused token %s at %s:%d", token, tokenLoc.filename, tokenLoc.lineNum)
		unusedTokenErrs = append(unusedTokenErrs, msg)
	}

	if len(unusedTokenErrs) > 0 {
		msg := strings.Join(unusedTokenErrs, "\n")
		if config.NoUnused {
			return errors.New(msg)
		} else {
			fmt.Println(msg)
		}
	}

	// Process groups
	if config.AckGroups {
		return ackTokenGroups(config, tokenMap)
	} else {
		return checkTokenGroups(tokenMap)
	}
}

func ackTokenGroups(config Config, tokenMap TokenMap) error {
	groupInfosByFile := map[string][]TokenGroupInfo{}

	for _, groupInfos := range tokenMap.Group {
		for _, groupInfo := range groupInfos {
			if groupInfo.actualHash != groupInfo.expectedHash {
				groupInfosByFile[groupInfo.fileSource.Filename] = append(groupInfosByFile[groupInfo.fileSource.Filename], groupInfo)
			}
		}
	}

	for _, groupInfos := range groupInfosByFile {
		err := ackTokenGroupsForFile(config, groupInfos)
		if err != nil {
			return err
		}
	}

	return nil
}

func ackTokenGroupsForFile(config Config, groupInfos []TokenGroupInfo) (err error) {
	fileSource := groupInfos[0].fileSource

	fileBytes, err := readFile(config, fileSource)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(fileSource.Filename, os.O_TRUNC|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	scn := bufio.NewScanner(bytes.NewReader(fileBytes))
	scn.Split(scanLinesWithNewlines)
	currentLine := 0
	for scn.Scan() {
		currentLine++
		lineBytes := scn.Bytes()
		for _, groupInfo := range groupInfos {
			if groupInfo.endLineNumber == currentLine {
				lineBytes = tokenGroupEndRegexp.ReplaceAll(
					lineBytes,
					[]byte(fmt.Sprintf(
						"[end-eyecue-codemap-group:%s:%s]",
						groupInfo.token,
						groupInfo.actualHash,
					)))
			}
		}

		_, err := file.Write(lineBytes)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkTokenGroups(tokenMap TokenMap) error {
	showAckMessage := false

	for groupName, groupInfos := range tokenMap.Group {
		showGroup := false

		for _, groupInfo := range groupInfos {
			if groupInfo.actualHash != groupInfo.expectedHash {
				showGroup = true
				break
			}
		}

		if showGroup {
			showAckMessage = true
			sort.Slice(groupInfos, func(i, j int) bool {
				return groupInfos[i].fileSource.Filename < groupInfos[j].fileSource.Filename
			})
			fmt.Printf("group \"%s\" has changes (indicated with *):\n", groupName)
			for _, groupInfo := range groupInfos {
				indicator := " "
				if groupInfo.actualHash != groupInfo.expectedHash {
					indicator = "*"
				}

				fmt.Printf("  %s  %s:%d (lines %d-%d)\n",
					indicator,
					groupInfo.fileSource.Filename,
					groupInfo.startLineNumber,
					groupInfo.startLineNumber+1,
					groupInfo.endLineNumber-1,
				)
			}
		}
	}

	if showAckMessage {
		return errors.New(`edit groups as needed, then run the "ack" command`)
	}

	return nil
}

func shouldIncludeFile(filename string) (bool, error) {
	stat, err := os.Lstat(filename)
	if err != nil {
		return false, fmt.Errorf(`failed to stat "%s": %w`, filename, err)
	}

	if stat.Mode().IsRegular() && stat.Size() < 10*1024*1024 {
		return true, nil
	}

	return false, nil
}

func readFilenamesFromStdin(nulDelimiter bool) ([]FileSource, error) {
	if isatty.IsTerminal(os.Stdin.Fd()) {
		fmt.Println("WARNING: reading filenames from stdin. Did you forget to pipe in a list of filenames?")
	}

	var fileSources []FileSource

	scn := bufio.NewScanner(os.Stdin)
	if nulDelimiter {
		scn.Split(scanNullDelimited)
	}
	for scn.Scan() {
		filename := strings.TrimPrefix(scn.Text(), "./")

		shouldInclude, err := shouldIncludeFile(filename)
		if err != nil {
			return nil, err
		}

		if shouldInclude {
			fileSources = append(fileSources, FileSource{
				Filename:     filename,
				FromGitIndex: false,
			})
		}
	}
	if scn.Err() != nil {
		return nil, fmt.Errorf("failed to read list of filenames from stdin: %w", scn.Err())
	}

	return fileSources, nil
}

func readFilenamesFromGit() ([]FileSource, error) {
	cmd := exec.Command("git", "diff-files", "--name-only", "--diff-filter=D", "-z")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run git diff-files: %s: %w", strings.TrimSpace(string(output)), err)
	}

	deletedFilenames := make(map[string]struct{})

	for _, filenameBytes := range bytes.Split(output, []byte{0}) {
		deletedFilenames[string(filenameBytes)] = struct{}{}
	}

	cmd = exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run git ls-files: %s: %w", strings.TrimSpace(string(output)), err)
	}

	var fileSources []FileSource

	for _, filenameBytes := range bytes.Split(output, []byte{0}) {
		filename := string(filenameBytes)
		if _, isDeleted := deletedFilenames[filename]; isDeleted {
			continue
		}

		shouldInclude, err := shouldIncludeFile(filename)
		if err != nil {
			return nil, err
		}

		if shouldInclude {
			fileSources = append(fileSources, FileSource{
				Filename:     filename,
				FromGitIndex: false,
			})
		}
	}

	return fileSources, nil
}

var spacesRegexp = regexp.MustCompile(`\s+`)

func readFilenamesFromGitIndex() ([]FileSource, error) {
	// Determine which files are modified+staged and must be read from the Git index vs. the working dir
	cmd := exec.Command("git", "diff-index", "--name-only", "-z", "HEAD")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run git diff-index: %s: %w", strings.TrimSpace(string(output)), err)
	}

	stagedFilenames := make(map[string]struct{})

	for _, filenameBytes := range bytes.Split(output, []byte{0}) {
		stagedFilenames[string(filenameBytes)] = struct{}{}
	}

	// Get a list of all filenames in the Git index
	cmd = exec.Command("git", "ls-files", "--stage", "-z")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run git ls-files: %s: %w", strings.TrimSpace(string(output)), err)
	}

	var fileSources []FileSource

	for _, lineBytes := range bytes.Split(output, []byte{0}) {
		line := string(lineBytes)

		// filter out non-files
		if !strings.HasPrefix(line, "100") {
			continue
		}

		// Output looks like:
		// 100644 b438169c25a6cf5649e09d8d51092998fa4e904e 0	Dockerfile
		parts := spacesRegexp.Split(line, 4)
		filename := parts[3]
		_, isStaged := stagedFilenames[filename]
		fileSources = append(fileSources, FileSource{
			Filename:     filename,
			FromGitIndex: isStaged,
		})
	}

	return fileSources, nil
}

func readFileFromGitIndex(filename string) ([]byte, error) {
	cmd := exec.Command("git", "show", ":"+filename)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git show :%s failed: %w", filename, err)
	}

	return output, err
}

func readFile(config Config, fileSource FileSource) ([]byte, error) {
	if fileSource.FromGitIndex {
		if config.Verbose {
			fmt.Printf("git index: reading \"%s\"\n", fileSource.Filename)
		}
		return readFileFromGitIndex(fileSource.Filename)
	}

	if config.Verbose {
		fmt.Printf("working dir: reading \"%s\"\n", fileSource.Filename)
	}
	return os.ReadFile(fileSource.Filename)
}

func processTokenGroups(fileSource FileSource, fileBytes []byte, tokenMap TokenMap) error {
	type CurrentGroup struct {
		Hasher          hash.Hash
		Token           string
		StartLineNumber int
	}
	var currentGroup *CurrentGroup

	currentLine := 1

	scn := bufio.NewScanner(bytes.NewReader(fileBytes))
	scn.Split(scanLinesWithNewlines)
	for scn.Scan() {
		line := scn.Text()

		groupMatch := tokenGroupEndRegexp.FindStringSubmatch(line)
		if len(groupMatch) > 0 {
			token := groupMatch[1]
			expectedHash := groupMatch[3]

			if currentGroup == nil {
				return fmt.Errorf(`end-eyecue-codemap-group for unknown group "%s" (%s:%d)`, token, fileSource.Filename, currentLine)
			}

			tokenMap.Group[token] = append(tokenMap.Group[token], TokenGroupInfo{
				token:           token,
				fileSource:      fileSource,
				startLineNumber: currentGroup.StartLineNumber,
				endLineNumber:   currentLine,
				actualHash:      fmt.Sprintf("%x", currentGroup.Hasher.Sum(nil)),
				expectedHash:    expectedHash,
			})

			currentGroup = nil
		}

		if currentGroup != nil {
			_, err := currentGroup.Hasher.Write(scn.Bytes())
			if err != nil {
				return err
			}
		}

		groupMatch = tokenGroupStartRegexp.FindStringSubmatch(line)
		if len(groupMatch) > 0 {
			token := groupMatch[1]
			if currentGroup != nil {
				return fmt.Errorf(`overlapping eyecue-codemap-group "%s" not allowed (%s:%d)`, token, fileSource.Filename, currentLine)
			}

			currentGroup = &CurrentGroup{
				Hasher:          sha1.New(),
				Token:           token,
				StartLineNumber: currentLine,
			}
		}

		currentLine++
	}

	if currentGroup != nil {
		return fmt.Errorf("unclosed eyecue-codemap-group in %s", fileSource.Filename)
	}

	return nil
}

func processFile(config Config, fileSource FileSource, tokenMap TokenMap) error {
	for _, ext := range ignoreExtensions {
		if strings.HasSuffix(fileSource.Filename, ext) {
			return nil
		}
	}

	fileBytes, err := readFile(config, fileSource)
	if err != nil {
		return fmt.Errorf(`failed to read "%s": %w`, fileSource.Filename, err)
	}

	// generate tokens
	if !config.CheckOnly {
		changed := false
		fileBytes = tokenNeededRegexp.ReplaceAllFunc(fileBytes, func(matched []byte) []byte {
			changed = true
			token := generateToken()
			fmt.Printf("Added new token \"%s\" to \"%s\"\n", token, fileSource.Filename)
			return []byte("[" + string(matched[1:len(matched)-1]) + ":" + token + "]")
		})

		if changed {
			err := os.WriteFile(fileSource.Filename, fileBytes, 0)
			if err != nil {
				return fmt.Errorf(`failed to write "%s": %w`, fileSource.Filename, err)
			}
		}
	}

	err = processTokenGroups(fileSource, fileBytes, tokenMap)
	if err != nil {
		return err
	}

	// We'll link to the entire file (instead of a specific line) for any [eyecue-codemap] that:
	// * Is preceded only by the shebang and/or blank lines
	// * Is followed by a blank line or EOF
	linkToFile := true

	// inventory tokens
	currentLine := 1
	var line string
	var peekLine bool
	scn := bufio.NewScanner(bytes.NewReader(fileBytes))
	for {
		if peekLine {
			peekLine = false
		} else {
			if !scn.Scan() {
				break
			}
			line = scn.Text()
		}

		doneScanning := false

		m := tokenRegexp.FindAllStringSubmatch(line, -1)

		// If we found a token, and we still think we want to link to the file,
		// check the next line to make sure it's blank or EOF.
		if linkToFile && len(m) > 0 {
			if scn.Scan() {
				peekLine = true
				line = scn.Text()
				if strings.TrimSpace(line) != "" {
					linkToFile = false
				}
			} else {
				// no more lines, we'll link to the file
				doneScanning = true
			}
		}

		for _, match := range m {
			before := strings.TrimSpace(string(match[1]))
			token := string(match[2])
			after := strings.TrimSpace(string(match[3]))

			// If the only thing on the line is the codemap comment,
			// link to the next line. Add more comment strings here as needed.
			lineNum := currentLine
			if after == "" && (before == "//" || before == "#") {
				lineNum++
			} else if before == "<!--" && after == "-->" {
				lineNum++
			}

			tokenMap.Single[token] = append(tokenMap.Single[token], TokenLocation{
				filename:   fileSource.Filename,
				lineNum:    lineNum,
				linkToFile: linkToFile,
			})
		}

		if doneScanning {
			break
		}

		if linkToFile && !(strings.HasPrefix(line, "#!") || strings.TrimSpace(line) == "") {
			linkToFile = false
		}

		currentLine++
	}
	if scn.Err() != nil {
		if scn.Err() == bufio.ErrTooLong {
			// probably not a text file. This is OK.
			return nil
		}

		return fmt.Errorf(`failed to scan "%s": %w`, fileSource.Filename, scn.Err())
	}

	return nil
}

func processMarkdownFile(config Config, mdFileSource FileSource, tokenMap TokenMap, unusedTokens map[string]struct{}) error {
	mdFilenameDir := filepath.Dir(mdFileSource.Filename)

	fileBytes, err := readFile(config, mdFileSource)
	if err != nil {
		return fmt.Errorf(`failed to read "%s": %w`, mdFileSource.Filename, err)
	}

	changed := false
	var newFileBytes bytes.Buffer

	lastLine := false
	for lineNum := 1; !lastLine; lineNum++ {
		var lineBytes []byte
		newLineIndex := bytes.IndexByte(fileBytes, '\n')
		if newLineIndex == -1 {
			lineBytes = fileBytes
			lastLine = true
		} else {
			lineBytes = fileBytes[:newLineIndex+1]
			fileBytes = fileBytes[newLineIndex+1:]
		}

		var replaceErr error
		lineBytes = tokenRefRegexp.ReplaceAllFunc(lineBytes, func(m []byte) []byte {
			if replaceErr != nil {
				return m
			}

			tokenIndex := bytes.IndexByte(m, ':') + 1
			tokenEndIndex := tokenIndex + bytes.IndexByte(m[tokenIndex:], '-')
			token := string(m[tokenIndex:tokenEndIndex])

			tokenLocs := tokenMap.Single[token]
			if len(tokenLocs) == 0 {
				replaceErr = fmt.Errorf(`token "%s" at "%s:%d" was not found`, token, mdFileSource.Filename, lineNum)
			} else {
				delete(unusedTokens, token)

				loc := tokenLocs[0]
				locRelPath, err := filepath.Rel(mdFilenameDir, loc.filename)
				if err != nil {
					replaceErr = fmt.Errorf("filepath.Rel(%s, %s): %w", mdFilenameDir, loc.filename, err)
				} else {
					original := string(m)
					var mdTarget string
					var outputTarget string
					if loc.linkToFile {
						mdTarget = locRelPath
						outputTarget = locRelPath
					} else {
						mdTarget = fmt.Sprintf("%s#L%d", locRelPath, loc.lineNum)
						outputTarget = fmt.Sprintf("%s:%d", locRelPath, loc.lineNum)
					}
					replacement := fmt.Sprintf("<!--eyecue-codemap:%s-->](%s)", token, mdTarget)
					if original != replacement {
						if config.CheckOnly {
							replaceErr = fmt.Errorf(`incorrect link at "%s:%d" token "%s"`, mdFileSource.Filename, lineNum, token)
						} else {
							changed = true
							fmt.Printf("updated link at \"%s:%d\" token \"%s\" -> \"%s\"\n", mdFileSource.Filename, lineNum, token, outputTarget)
							return []byte(replacement)
						}
					}
				}
			}

			return m
		})

		if replaceErr != nil {
			return replaceErr
		}

		_, err = newFileBytes.Write(lineBytes)
		if err != nil {
			return err
		}
	}

	if changed {
		err := os.WriteFile(mdFileSource.Filename, newFileBytes.Bytes(), 0)
		if err != nil {
			return fmt.Errorf(`failed to write "%s": %w`, mdFileSource.Filename, err)
		}
	}

	return nil
}

func generateToken() string {
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	if err != nil {
		panic(fmt.Errorf("failed to read random bytes: %w", err))
	}

	return base58.Encode(buf)
}
