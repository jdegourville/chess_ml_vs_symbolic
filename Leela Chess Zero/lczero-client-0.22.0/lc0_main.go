// A new client to work with the lc0 binary.
//
//
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"client"

	"github.com/Tilps/chess"
	"github.com/nightlyone/lockfile"
)

var (
	startTime       time.Time
	totalGames      int
	pendingNextGame *client.NextGameResponse
	randId          int
	hasCudnn        bool
	hasCudnnFp16    bool
	hasOpenCL       bool
	hasBlas         bool
	testedCudnnFp16 bool

	hostname = flag.String("hostname", "http://api.lczero.org", "Address of the server")
	user     = flag.String("user", "", "Username")
	password = flag.String("password", "", "Password")
	gpu      = flag.Int("gpu", -1, "GPU to use (ignored if --backend-opts used)")
	//	debug    = flag.Bool("debug", false, "Enable debug mode to see verbose output and save logs")
	lc0Args  = flag.String("lc0args", "", "")
	backopts = flag.String("backend-opts", "",
		`Options for the lc0 mux. backend. Example: --backend-opts="cudnn(gpu=1)"`)
	parallel      = flag.Int("parallelism", -1, "Number of games to play in parallel (-1 for default)")
	useTestServer = flag.Bool("use-test-server", false, "Set host name to test server.")
	runId         = flag.Uint("run", 0, "Which training run to contribute to (default 0 to let server decide)")
	keep          = flag.Bool("keep", false, "Do not delete old network files")
	version       = flag.Bool("version", false, "Print version and exit.")
	trainOnly     = flag.Bool("train-only", false, "Do not play match games")
)

// Settings holds username and password.
type Settings struct {
	User string
	Pass string
}

const inf = "inf"

/*
	Reads the user and password from a config file and returns empty strings if anything went wrong.
	If the config file does not exists, it prompts the user for a username and password and creates the config file.
*/
func readSettings(path string) (string, string) {
	settings := Settings{}
	file, err := os.Open(path)
	if err != nil {
		// File was not found
		fmt.Printf("Please enter your username and password, an account will be automatically created.\n")
		fmt.Printf("Note that this password will be stored in plain text, so avoid a password that is\n")
		fmt.Printf("also used for sensitive applications. It also cannot be recovered.\n")
		fmt.Printf("Enter username : ")
		fmt.Scanf("%s\n", &settings.User)
		fmt.Printf("Enter password : ")
		fmt.Scanf("%s\n", &settings.Pass)
		jsonSettings, err := json.Marshal(settings)
		if err != nil {
			log.Fatal("Cannot encode settings to JSON ", err)
			return "", ""
		}
		settingsFile, err := os.Create(path)
		defer settingsFile.Close()
		if err != nil {
			log.Fatal("Could not create output file ", err)
			return "", ""
		}
		settingsFile.Write(jsonSettings)
		return settings.User, settings.Pass
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&settings)
	if err != nil {
		log.Fatal("Error decoding JSON ", err)
		return "", ""
	}
	return settings.User, settings.Pass
}

func getExtraParams() map[string]string {
	return map[string]string{
		"user":       *user,
		"password":   *password,
		"version":    "22",
		"token":      strconv.Itoa(randId),
		"train_only": strconv.FormatBool(*trainOnly),
	}
}

func uploadGame(httpClient *http.Client, path string, pgn string,
	nextGame client.NextGameResponse, version string, fp_threshold float64) error {

	var retryCount uint32

	for {
		retryCount++
		if retryCount > 3 {
			return errors.New("UploadGame failed: Too many retries")
		}

		extraParams := getExtraParams()
		extraParams["training_id"] = strconv.Itoa(int(nextGame.TrainingId))
		extraParams["network_id"] = strconv.Itoa(int(nextGame.NetworkId))
		extraParams["pgn"] = pgn
		extraParams["engineVersion"] = version
		if fp_threshold >= 0.0 {
			extraParams["fp_threshold"] = strconv.FormatFloat(fp_threshold, 'E', -1, 64)
		}
		request, err := client.BuildUploadRequest(*hostname+"/upload_game", extraParams, "file", path)
		if err != nil {
			log.Printf("BUR: %v", err)
			return err
		}
		resp, err := httpClient.Do(request)
		if err != nil {
			log.Printf("http.Do: %v", err)
			return err
		}
		body := &bytes.Buffer{}
		_, err = body.ReadFrom(resp.Body)
		if err != nil {
			log.Print(err)
			log.Print("Error uploading, retrying...")
			time.Sleep(time.Second * (2 << retryCount))
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 200 && strings.Contains(body.String(), " upgrade ") {
			log.Fatal("The lc0 version you are using is not accepted by the server")
		}
		break
	}

	totalGames++
	log.Printf("Completed %d games in %s time", totalGames, time.Since(startTime))

	err := os.Remove(path)
	if err != nil {
		log.Printf("Failed to remove training file: %v", err)
	}

	return nil
}

type gameInfo struct {
	pgn   string
	fname string
	// If >= 0, this is the value which if resign threshold was set
	// higher a false positive would have occurred if the game had been
	// played with resign.
	fp_threshold float64
	player1      string
	result       string
}

type cmdWrapper struct {
	Cmd      *exec.Cmd
	Pgn      string
	Input    io.WriteCloser
	BestMove chan string
	gi       chan gameInfo
	Version  string
	Retry    chan bool
}

func (c *cmdWrapper) openInput() {
	var err error
	c.Input, err = c.Cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
}

func convertMovesToPGN(moves []string, result string) string {
	game := chess.NewGame(chess.UseNotation(chess.LongAlgebraicNotation{}))
	for _, m := range moves {
		err := game.MoveStr(m)
		if err != nil {
			log.Fatalf("movstr: %v", err)
		}
	}
	if game.Outcome() == chess.NoOutcome && len(game.EligibleDraws()) > 1 {
		game.Draw(game.EligibleDraws()[1])
	}
	game2 := chess.NewGame()
	b, err := game.MarshalText()
	if err != nil {
		log.Fatalf("MarshalText failed: %v", err)
	}
	b_str := string(b)
	if strings.HasSuffix(b_str, " *") && result != "" {
		to_append := "1/2-1/2"
		if result == "whitewon" {
			to_append = "1-0"
		} else if result == "blackwon" {
			to_append = "0-1"
		}	
		b = []byte(strings.TrimRight(b_str, "*") + to_append)
	}
	game2.UnmarshalText(b)
	return game2.String()
}

func createCmdWrapper() *cmdWrapper {
	c := &cmdWrapper{
		gi:       make(chan gameInfo),
		BestMove: make(chan string),
		Version:  "v0.10.0",
		Retry:    make(chan bool),
	}
	return c
}

func checkLc0() {
	dir, _ := os.Getwd()
	cmd := exec.Command(path.Join(dir, "lc0"))
	cmd.Args = append(cmd.Args, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatal(err)
	}
	if bytes.Contains(out, []byte("blas")) {
		hasBlas = true
	}
	if bytes.Contains(out, []byte("cudnn-fp16")) {
		hasCudnnFp16 = true
	}
	if bytes.Contains(out, []byte("cudnn")) {
		hasCudnn = true
		if hasCudnnFp16 && bytes.Index(out, []byte("cudnn")) == bytes.LastIndex(out, []byte("cudnn")) {
			hasCudnn = false
		}
	}
	if bytes.Contains(out, []byte("opencl")) {
		hasOpenCL = true
	}
}

func (c *cmdWrapper) launch(networkPath string, otherNetPath string, args []string, input bool) {
	dir, _ := os.Getwd()
	c.Cmd = exec.Command(path.Join(dir, "lc0"))
	// Add the "selfplay" or "uci" part first
	mode := args[0]
	c.Cmd.Args = append(c.Cmd.Args, mode)
	args = args[1:]
	if mode != "selfplay" {
		c.Cmd.Args = append(c.Cmd.Args, "--backend=multiplexing")
	}
	if *lc0Args != "" {
		log.Println("WARNING: Option --lc0args is for testing, not production use!")
		log.SetPrefix("TESTING: ")
		parts := strings.Split(*lc0Args, " ")
		c.Cmd.Args = append(c.Cmd.Args, parts...)
	}
	parallelism := *parallel
	sGpu := ""
	if *gpu >= 0 {
		sGpu = fmt.Sprintf(",gpu=%v", *gpu)
	}
	if *backopts != "" {
		// Check agains small token blacklist, currently only "random"
		tokens := regexp.MustCompile("[,=().0-9]").Split(*backopts, -1)
		for _, token := range tokens {
			switch token {
			case "random":
				log.Fatalf("Not accepted in --backend-opts: %s", token)
			}
		}
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--backend-opts=%s", *backopts))
	} else if hasCudnnFp16 {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--backend-opts=backend=cudnn-fp16%v", sGpu))
		if parallelism <= 0 {
			parallelism = 32
		}
	} else if hasCudnn {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--backend-opts=backend=cudnn%v", sGpu))
	} else if hasOpenCL {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--backend-opts=backend=opencl%v", sGpu))
	}
	if parallelism > 0 && mode == "selfplay" {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--parallelism=%v", parallelism))
	}
	c.Cmd.Args = append(c.Cmd.Args, args...)
	if otherNetPath == "" {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--weights=%s", networkPath))
	} else {
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--player1.weights=%s", networkPath))
		c.Cmd.Args = append(c.Cmd.Args, fmt.Sprintf("--player2.weights=%s", otherNetPath))
		c.Cmd.Args = append(c.Cmd.Args, "--no-share-trees")
	}

	fmt.Printf("Args: %v\n", c.Cmd.Args)

	stdout, err := c.Cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	c.Cmd.Stderr = c.Cmd.Stdout

	// If the game wasn't played with resign, and the engine supports it,
	// this will be populated by the resign_report before the gameready
	// with the value which the resign threshold should be kept below to
	// avoid a false positive.
	last_fp_threshold := -1.0
	go func() {
		defer close(c.BestMove)
		defer close(c.gi)
		stdoutScanner := bufio.NewScanner(stdout)
		for stdoutScanner.Scan() {
			line := stdoutScanner.Text()
			//			fmt.Printf("lc0: %s\n", line)
			switch {
			case strings.Contains(line, "Your GPU doesn't support FP16"):
				log.Println("GPU doesn't support the cudnn-fp16 backend")
				if *backopts == "" {
					hasCudnnFp16 = false
					c.Retry <- true
				} else {
					log.Fatal("Terminating")
				}
			case strings.HasPrefix(line, "resign_report "):
				args := strings.Split(line, " ")
				fp_threshold_idx := -1
				for idx, arg := range args {
					if arg == "fp_threshold" {
						fp_threshold_idx = idx + 1
					}
				}
				if fp_threshold_idx >= 0 {
					last_fp_threshold, err = strconv.ParseFloat(args[fp_threshold_idx], 64)
					if err != nil {
						log.Printf("Malformed resign_report: %q", line)
						last_fp_threshold = -1.0
					}
				}
				fmt.Println(line)
			case strings.HasPrefix(line, "gameready "):
				// filename is between "trainingfile" and "gameid"
				idx1 := strings.Index(line, "trainingfile")
				idx2 := strings.LastIndex(line, "gameid")
				idx3 := strings.LastIndex(line, "moves")
				if idx1 < 0 || idx2 < 0 || idx3 < 0 {
					log.Printf("Malformed gameready: %q", line)
					break
				}
				idx4 := strings.LastIndex(line, "player1")
				idx5 := strings.LastIndex(line, "result")
				result := ""
				if idx5 < 0 {
					idx5 = idx3
				} else {
					result = line[idx5+7 : idx3-1]
				}
				player := ""
				if idx4 >= 0 {
					player = line[idx4+8 : idx5-1]
				}
				file := line[idx1+13 : idx2-1]
				pgn := convertMovesToPGN(strings.Split(line[idx3+6:len(line)], " "), result)
				fmt.Printf("PGN: %s\n", pgn)
				c.gi <- gameInfo{pgn: pgn, fname: file, fp_threshold: last_fp_threshold, player1: player, result: result}
				last_fp_threshold = -1.0
			case strings.HasPrefix(line, "bestmove "):
				//				fmt.Println(line)
				testedCudnnFp16 = true
				c.BestMove <- strings.Split(line, " ")[1]
			case strings.HasPrefix(line, "id name Lc0 "):
				c.Version = strings.Split(line, " ")[3]
				fmt.Println(line)
			case strings.HasPrefix(line, "info"):
				testedCudnnFp16 = true
			case strings.HasPrefix(line, "GPU compute capability:"):
				cc, _ := strconv.ParseFloat(strings.Split(line, " ")[3], 32)
				if cc >= 7.0 {
					testedCudnnFp16 = true
				}
				fallthrough
			default:
				fmt.Println(line)
			}
		}
	}()

	if input {
		c.openInput()
	}

	err = c.Cmd.Start()
	if err != nil {
		log.Fatal(err)
	}
}

func resultToNum(result string) int {
	if result == "whitewon" {
		return 1
	}
	if result == "blackwon" {
		return -1
	}
	return 0
}

func playMatch(httpClient *http.Client, ngr client.NextGameResponse, baselinePath string, candidatePath string, params []string) (*client.NextGameResponse, error) {
	// lc0 needs selfplay first in the argument list.
	params = append([]string{"selfplay"}, params...)
	// Training flag used for simplicity for now.
	params = append(params, "--training=true")
	hasVisitsParam := false
	for i := range params {
		if strings.HasPrefix(params[i], "--visits=") || strings.HasPrefix(params[i], "--playouts=") {
			hasVisitsParam = true
		}
	}
	if !hasVisitsParam {
		params = append(params, "--visits=800")
	}
	c := createCmdWrapper()
	c.launch(candidatePath, baselinePath, params /* input= */, false)
	trainDirHolder := make([]string, 1)
	trainDirHolder[0] = ""
	defer func() {
		// Remove the training dir when we're done training.
		trainDir := trainDirHolder[0]
		if trainDir != "" {
			log.Printf("Removing traindir: %s", trainDir)
			err := os.RemoveAll(trainDir)
			if err != nil {
				log.Printf("Error removing train dir: %v", err)
			}
		}
	}()
	doneCh := make(chan bool)
	gameInfoCh := make(chan gameInfo)
	reverseDoneCh := make(chan bool)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	var pendingNextGame *client.NextGameResponse
	go func() {
		defer wg.Done()
		defer close(doneCh)
		errCount := 0
		curng := &ngr
		var flipped []gameInfo
		var normal []gameInfo
		for done := false; !done; {
			select {
			case <-reverseDoneCh:
				log.Println("Match uploader exiting")
				return
			case gi, _ := <-gameInfoCh:
				if gi.player1 == "black" {
					flipped = append(flipped, gi)
				} else {
					normal = append(normal, gi)
				}
				for true {
					if curng != nil {
						if curng.Flip && len(flipped) > 0 {
							l := len(flipped)
							nextgi := flipped[l-1]
							flipped = flipped[:l-1]
							log.Println("uploading match result")
							extraParams := getExtraParams()
							extraParams["engineVersion"] = c.Version
							client.UploadMatchResult(httpClient, *hostname, curng.MatchGameId, -resultToNum(nextgi.result), nextgi.pgn, extraParams)
							log.Println("uploaded")
							curng = nil
						} else if !curng.Flip && len(normal) > 0 {
							l := len(normal)
							nextgi := normal[l-1]
							normal = normal[:l-1]
							log.Println("uploading match result")
							extraParams := getExtraParams()
							extraParams["engineVersion"] = c.Version
							client.UploadMatchResult(httpClient, *hostname, curng.MatchGameId, resultToNum(nextgi.result), nextgi.pgn, extraParams)
							log.Println("uploaded")
							curng = nil
						}
					}
					if curng != nil {
						break
					}
					ng, err := client.NextGame(httpClient, *hostname, getExtraParams())
					if err != nil {
						fmt.Printf("Error talking to server: %v\n", err)
						errCount++
						if errCount < 10 {
							break
						}
						return
					}
					if ng.Type != ngr.Type || ng.Sha != ngr.Sha || ng.CandidateSha != ngr.CandidateSha {
						log.Println("Current match finished.")
						pendingNextGame = &ng
						return
					}
					curng = &ng
					errCount = 0
				}
			}
		}
	}()
	progressOrKill := false
	for done := false; !done; {
		select {
		case <-c.Retry:
			close(reverseDoneCh)
			return nil, errors.New("retry")
		case <-doneCh:
			done = true
			progressOrKill = true
			log.Println("Received message to end matches, killing lc0")
			c.Cmd.Process.Kill()
		case _, ok := <-c.BestMove:
			// Just swallow the best moves, not actually needed.
			if !ok {
				log.Printf("BestMove channel closed unexpectedly, exiting match loop")
				break
			}
		case gi, ok := <-c.gi:
			if !ok {
				// Under windows we don't get the exception, so also check here.
				if hasCudnnFp16 && !testedCudnnFp16 && *backopts == "" {
					log.Println("GPU probably doesn't support the cudnn-fp16 backend")
					hasCudnnFp16 = false
					close(reverseDoneCh)
					return nil, errors.New("retry")
				}
				log.Printf("GameInfo channel closed, exiting match loop")
				done = true
				break
			}
			testedCudnnFp16 = true
			progressOrKill = true
			trainDirHolder[0] = path.Dir(gi.fname)
			wg.Add(1)
			go func() {
				select {
				case <-doneCh:
				case gameInfoCh <- gi:
				}
				wg.Done()
			}()
		}
	}

	log.Println("Waiting for lc0 to stop")
	err := c.Cmd.Wait()
	if err != nil {
		fmt.Printf("lc0 exited with: %v", err)
	}
	log.Println("lc0 stopped")
	close(reverseDoneCh)

	log.Println("Waiting for uploads to complete")
	wg.Wait()
	if !progressOrKill {
		return nil, errors.New("Client self-exited without producing any matches.")
	}
	return pendingNextGame, nil
}

func train(httpClient *http.Client, ngr client.NextGameResponse,
	networkPath string, otherNetPath string, count int, params []string, doneCh chan bool) error {
	// lc0 needs selfplay first in the argument list.
	params = append([]string{"selfplay"}, params...)
	params = append(params, "--training=true")
	c := createCmdWrapper()
	c.launch(networkPath, otherNetPath, params /* input= */, false)
	trainDirHolder := make([]string, 1)
	trainDirHolder[0] = ""
	defer func() {
		// Remove the training dir when we're done training.
		trainDir := trainDirHolder[0]
		if trainDir != "" {
			log.Printf("Removing traindir: %s", trainDir)
			err := os.RemoveAll(trainDir)
			if err != nil {
				log.Printf("Error removing train dir: %v", err)
			}
		}
	}()
	wg := &sync.WaitGroup{}
	numGames := 1
	progressOrKill := false
	for done := false; !done; {
		select {
		case <-c.Retry:
			return errors.New("retry")
		case <-doneCh:
			done = true
			progressOrKill = true
			log.Println("Received message to end training, killing lc0")
			c.Cmd.Process.Kill()
		case _, ok := <-c.BestMove:
			// Just swallow the best moves, only needed for match play.
			if !ok {
				log.Printf("BestMove channel closed unexpectedly, exiting train loop")
				break
			}
		case gi, ok := <-c.gi:
			if !ok {
				// Under windows we don't get the exception, so also check here.
				if hasCudnnFp16 && !testedCudnnFp16 && *backopts == "" {
					log.Println("GPU probably doesn't support the cudnn-fp16 backend")
					hasCudnnFp16 = false
					return errors.New("retry")
				}
				log.Printf("GameInfo channel closed, exiting train loop")
				done = true
				break
			}
			testedCudnnFp16 = true
			fmt.Printf("Uploading game: %d\n", numGames)
			numGames++
			progressOrKill = true
			trainDirHolder[0] = path.Dir(gi.fname)
			log.Printf("trainDir=%s", trainDirHolder[0])
			wg.Add(1)
			go func() {
				uploadGame(httpClient, gi.fname, gi.pgn, ngr, c.Version, gi.fp_threshold)
				wg.Done()
			}()
		}
	}

	log.Println("Waiting for lc0 to stop")
	err := c.Cmd.Wait()
	if err != nil {
		fmt.Printf("lc0 exited with: %v", err)
	}
	log.Println("lc0 stopped")

	log.Println("Waiting for uploads to complete")
	wg.Wait()
	if !progressOrKill {
		return errors.New("Client self-exited without producing any games.")
	}
	return nil
}

func checkValidNetwork(dir string, sha string) (string, error) {
	// Sha already exists?
	path := filepath.Join(dir, sha)
	_, err := os.Stat(path)
	if err == nil {
		file, _ := os.Open(path)
		reader, err := gzip.NewReader(file)
		if err == nil {
			_, err = ioutil.ReadAll(reader)
		}
		file.Close()
		if err != nil {
			fmt.Printf("Deleting invalid network...\n")
			os.Remove(path)
			return path, err
		} else {
			return path, nil
		}
	}
	return path, err
}

func removeAllExcept(dir string, sha string, keepTime string) error {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.Name() == sha {
			continue
		}
		timeLimit, _ := time.ParseDuration(keepTime)
		if time.Since(file.ModTime()) < timeLimit {
			continue
		}
		fmt.Printf("Removing %v\n", file.Name())
		err := os.RemoveAll(filepath.Join(dir, file.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

func acquireLock(dir string, sha string) (lockfile.Lockfile, error) {
	lockpath, _ := filepath.Abs(filepath.Join(dir, sha+".lck"))
	lock, err := lockfile.New(lockpath)
	if err != nil {
		// Unknown error. Exit.
		log.Fatalf("Cannot init lockfile: %v", err)
	}
	// Attempt to acquire lock
	err = lock.TryLock()
	return lock, err
}

func getNetwork(httpClient *http.Client, sha string, keepTime string) (string, error) {
	dir := "networks"
	os.MkdirAll(dir, os.ModePerm)
	if keepTime != inf {
		err := removeAllExcept(dir, sha, keepTime)
		if err != nil {
			log.Printf("Failed to remove old network(s): %v", err)
		}
	}
	path, err := checkValidNetwork(dir, sha)
	if err == nil {
		// There is already a valid network. Use it.
		return path, nil
	}

	// Otherwise, let's download it
	lock, err := acquireLock(dir, sha)

	if err != nil {
		if err == lockfile.ErrBusy {
			log.Println("Download initiated by other client")
			return "", err
		} else {
			log.Fatalf("Unable to lock: %v", err)
		}
	}

	// Lockfile acquired, download it
	defer lock.Unlock()
	fmt.Println("Downloading network...")
	err = client.DownloadNetwork(httpClient, *hostname, path, sha)
	if err != nil {
		log.Printf("Network download failed: %v", err)
		return "", err
	}
	return checkValidNetwork(dir, sha)
}

func nextGame(httpClient *http.Client, count int) error {
	var nextGame client.NextGameResponse
	var err error
	if pendingNextGame != nil {
		nextGame = *pendingNextGame
		pendingNextGame = nil
		err = nil
	} else {
		nextGame, err = client.NextGame(httpClient, *hostname, getExtraParams())
		if err != nil {
			return err
		}
	}
	var serverParams []string
	err = json.Unmarshal([]byte(nextGame.Params), &serverParams)
	if err != nil {
		return err
	}
	log.Printf("serverParams: %s", serverParams)

	if nextGame.Type == "match" {
		log.Println("Getting networks for match")
		networkPath, err := getNetwork(httpClient, nextGame.Sha, inf)
		if err != nil {
			return err
		}
		candidatePath, err := getNetwork(httpClient, nextGame.CandidateSha, inf)
		if err != nil {
			return err
		}
		log.Println("Starting match")
		possibleNextGame, err := playMatch(httpClient, nextGame, networkPath, candidatePath, serverParams)
		if err != nil {
			log.Printf("playMatch: %v", err)
			return err
		}
		pendingNextGame = possibleNextGame
		return nil
	}

	if nextGame.Type == "train" {
		keepTime := nextGame.KeepTime
		if *keep {
			keepTime = inf
		} else if keepTime == "" {
			// Four hours should be enough for clients serving 2 parallel runs in
			// the same directory, even after one or two failed failed promotions.
			keepTime = "4h"
		}
		networkPath, err := getNetwork(httpClient, nextGame.Sha, keepTime)
		if err != nil {
			return err
		}
		otherNetPath := ""
		if nextGame.CandidateSha != "" {
			otherNetPath, err = getNetwork(httpClient, nextGame.CandidateSha, inf)
			if err != nil {
				return err
			}
		}
		doneCh := make(chan bool)
		go func() {
			defer close(doneCh)
			errCount := 0
			for {
				time.Sleep(60 * time.Second)
				if nextGame.Type == "Done" {
					return
				}
				ng, err := client.NextGame(httpClient, *hostname, getExtraParams())
				if err != nil {
					fmt.Printf("Error talking to server: %v\n", err)
					errCount++
					if errCount < 10 {
						continue
					}
					return
				}
				if ng.Type != nextGame.Type || ng.Sha != nextGame.Sha {
					if ng.Type == "match" {
						// Prefetch the next net before terminating game.
						getNetwork(httpClient, ng.CandidateSha, inf)
					}
					pendingNextGame = &ng
					return
				}
				errCount = 0
			}
		}()
		err = train(httpClient, nextGame, networkPath, otherNetPath, count, serverParams, doneCh)
		// Ensure the anonymous function stops retrying.
		nextGame.Type = "Done"
		if err != nil {
			return err
		}
		return nil
	}

	return errors.New("Unknown game type: " + nextGame.Type)
}

// Check if PGN may contain "e.p." to verify that the chess package is recent
func testEP() {
	game := chess.NewGame(chess.UseNotation(chess.AlgebraicNotation{}))
	game.MoveStr("a4")
	game.MoveStr("c5")
	game.MoveStr("a5")
	game.MoveStr("b5")
	game.MoveStr("axb6")

	if strings.Contains(game.String(), "e.p.") {
		log.Fatal("You need a more recent version of package github.com/Tilps/chess")
	}
}

func hideLc0argsFlag() {
	shown := new(flag.FlagSet)
	flag.VisitAll(func(f *flag.Flag) {
		if f.Name != "lc0args" {
			shown.Var(f.Value, f.Name, f.Usage)
		}
	})
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		shown.PrintDefaults()
	}
}

func maybeSetTrainOnly() {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "train-only" {
			found = true
		}
	})
	if !found && !hasCudnn && !hasCudnnFp16 {
		*trainOnly = true
		log.Println("Will only run training games, use -train-only=false to override")
	}
}

func main() {
	fmt.Printf("Lc0 client version %v\n", getExtraParams()["version"])

	testEP()

	hideLc0argsFlag()
	flag.Parse()

	if *version {
		return
	}

	checkLc0()

	maybeSetTrainOnly()

	// 640 ought to be enough for anybody.
	if *runId > 640 {
		log.Fatal("Training run number too large")
	}
	randBytes := make([]byte, 2)
	_, err := rand.Reader.Read(randBytes)
	if err != nil {
		randId = -1
	} else {
		randId = int(*runId)<<16 | int(randBytes[0])<<8 | int(randBytes[1])
	}

	if *useTestServer {
		*hostname = "http://testserver.lczero.org"
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if len(*user) == 0 || len(*password) == 0 {
		*user, *password = readSettings("settings.json")
	}

	if len(*user) == 0 {
		log.Fatal("You must specify a username")
	}
	if len(*password) == 0 {
		log.Fatal("You must specify a non-empty password")
	}

	httpClient := &http.Client{}
	startTime = time.Now()
	for i := 0; ; i++ {
		err := nextGame(httpClient, i)
		if err != nil {
			if err.Error() == "retry" {
				time.Sleep(1 * time.Second)
				continue
			}
			log.Print(err)
			log.Print("Sleeping for 30 seconds...")
			time.Sleep(30 * time.Second)
			continue
		}
	}
}
