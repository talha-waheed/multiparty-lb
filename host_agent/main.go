package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CC_SERVER_HOST              = "localhost"
	CC_SERVER_PORT              = 9988
	CC_SERVER_TYPE              = "tcp"
	LB_SERVER_PORT              = 9989
	CPU_UTILIZATION_INTERVAL_MS = 100
	DEFAULT_LB_WEIGHTS          = ""
)

/*
What does this server do:

1. Listen for connections from CC
2. When a connection is received, handle the connection in a new goroutine
3. In the goroutine, read the message from the connection
4. If the message is an request to update the pod state, update agent's pod
	state, and send a success/failure response
	(state would contain list of podnames to uid mappings in the node)
4. If the message is a request for the server to apply CPU shares,
	apply the CPU shares in the kernel, and send a success/failure response
5. If the message is a request for the server to get CPU utilizations,
	send the CPU utilizations for each pod
6. Repeat from 3. indefinitely (until connection is closed)
*/

type SafeLBWeights struct {
	mu      sync.Mutex
	weights string
}

func main() {

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// keep LB weights as a shared variable managed by a seperate go routine
	lbWeights := &SafeLBWeights{
		weights: DEFAULT_LB_WEIGHTS}

	// start the server that will communicate with the central controller
	go startServerForCC(lbWeights)

	// listen for requests from the load balancer for updating its weights
	listenForReqsFromLB(lbWeights)
}

func startServerForCC(lbWeights *SafeLBWeights) {

	fmt.Println("Server Running...")

	server, err := net.Listen(
		CC_SERVER_TYPE, fmt.Sprintf(":%d", CC_SERVER_PORT))
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		os.Exit(1)
	}

	defer server.Close()

	fmt.Println(
		"Listening on " + CC_SERVER_HOST + fmt.Sprintf(":%d", CC_SERVER_PORT))
	fmt.Println("Waiting for client...")

	for {
		connection, err := server.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		fmt.Println("client connected")
		go processClient(connection, lbWeights)
	}

}

func listenForReqsFromLB(lbWeights *SafeLBWeights) {
	// listen for http requests at a specific port
	// and update the LB weights

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Connection", "close")

		lbWeights.mu.Lock()
		currLBWeights := lbWeights.weights
		lbWeights.mu.Unlock()

		// get post body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading request body",
				http.StatusInternalServerError)
			return
		}
		fmt.Println("Received request: " + string(body))

		fmt.Fprint(w, currLBWeights)
	})

	fmt.Printf(
		"Server running (port=%d), route: http://localhost:%d\n",
		LB_SERVER_PORT, LB_SERVER_PORT)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", LB_SERVER_PORT), nil); err != nil {
		log.Fatal(err)
	}

}

func processClient(connection net.Conn, lbWeights *SafeLBWeights) {

	defer connection.Close()

	podUIDs := make(map[string]string)

	for {

		msgFromCC, err := readMsgFromConnection(connection)
		if err != nil {
			fmt.Println("Error reading:", err.Error())
			break
		}
		slog.Info("Received: " + msgFromCC)

		msgType := strings.Split(msgFromCC, " ")[0]

		if msgType == "updatePods" {
			newPodUIDs, ok := getNewPods(msgFromCC)
			if ok {
				podUIDs = newPodUIDs
			}
			sendSuccessOrFailResponse(connection, ok)

		} else if msgType == "applyLBWeights" {
			ok := updateLBWeights(podUIDs, msgFromCC, lbWeights)
			sendSuccessOrFailResponse(connection, ok)

		} else if msgType == "applyCPUShares" {
			ok := applyCPUShares(podUIDs, msgFromCC)
			sendSuccessOrFailResponse(connection, ok)

		} else if msgType == "applyCPUQuotas" {
			ok := applyCPUQuotas(podUIDs, msgFromCC)
			sendSuccessOrFailResponse(connection, ok)

		} else if msgType == "getCPUUtilizations" {
			cpuUtilizations := getCPUUtilizations(podUIDs)
			sendMsgToConnection(connection, cpuUtilizations)

		} else {
			// unknown message type
			sendMsgToConnection(connection, "Unknown message type")
		}
	}

	slog.Warn("Client disconnected")
}

func getNewPods(msg string) (map[string]string, bool) {
	// parse the message and update the state
	// return true if successful, false otherwise

	// example message to parse: "updateState pod1:uid1 pod2:uid2"

	podUIDs := make(map[string]string)
	podStrs := strings.Split(msg, " ")[1:]
	for _, podStr := range podStrs {
		podNameToUID := strings.Split(podStr, ":")
		if len(podNameToUID) != 2 {
			return podUIDs, false
		}
		podUIDs[podNameToUID[0]] = podNameToUID[1]
	}

	slog.Info("Updated pods: " + fmt.Sprintf("%v", podUIDs))

	return podUIDs, true
}

func applyCPUQuotas(podUIDs map[string]string, msg string) bool {
	// parse the message and apple CPU quota
	// return true if successful, false otherwise

	podQuotas, ok := parsePodShares(msg)
	if !ok {
		return false
	}

	for podName, share := range podQuotas {

		fileName := "/host/sys/fs/cgroup/cpu/kubepods/" +
			podUIDs[podName] + "/cpu.cfs_quota_us"
		// fileName := "/Users/twaheed2/go/src/host_agent/" +
		// 	podUIDs[podName]

		// err := os.WriteFile(fileName, []byte(share+"\n"), 0644)
		// if err != nil {
		// 	slog.Warn(err.Error())
		// 	return false
		// }

		slog.Info(fmt.Sprintf("%s %s %s %s",
			"bash", "./writetofile.sh", share, fileName))

		cmd := exec.Command("bash", "./writetofile.sh", share, fileName)
		_, err := cmd.Output()
		if err != nil {
			slog.Warn(err.Error())
			return false
		}
	}

	slog.Info("Applied CPU shares: " + fmt.Sprintf("%v", podQuotas))

	return true
}

func updateLBWeights(
	podUIDs map[string]string, msg string, lbWeights *SafeLBWeights) bool {
	// parse the message and update lb weights
	// return true if successful, false otherwise

	// newLBWeights, ok := parseLBWeights(msg)
	// if !ok {
	// 	return false
	// }

	lbWeights.mu.Lock()
	lbWeights.weights = msg
	lbWeights.mu.Unlock()

	slog.Info("Updated LB weights: " + msg)

	return true
}

func applyCPUShares(podUIDs map[string]string, msg string) bool {
	// parse the message and apple CPU shares
	// return true if successful, false otherwise

	podShares, ok := parsePodShares(msg)
	if !ok {
		return false
	}

	for podName, share := range podShares {

		fileName := "/host/sys/fs/cgroup/cpu/kubepods/" +
			podUIDs[podName] + "/cpu.shares"
		// fileName := "/Users/twaheed2/go/src/host_agent/" +
		// 	podUIDs[podName]

		// err := os.WriteFile(fileName, []byte(share+"\n"), 0644)
		// if err != nil {
		// 	slog.Warn(err.Error())
		// 	return false
		// }

		slog.Info(fmt.Sprintf("%s %s %s %s",
			"bash", "./writetofile.sh", share, fileName))

		cmd := exec.Command("bash", "./writetofile.sh", share, fileName)
		_, err := cmd.Output()
		if err != nil {
			slog.Warn(err.Error())
			return false
		}
	}

	slog.Info("Applied CPU shares: " + fmt.Sprintf("%v", podShares))

	return true
}

func parsePodShares(msg string) (map[string]string, bool) {

	// example message to parse: "applyCPUShares pod1:45 pod2:69"

	podShares := make(map[string]string)
	podStrs := strings.Split(msg, " ")[1:]
	for _, podStr := range podStrs {
		podNameToShare := strings.Split(podStr, ":")
		if len(podNameToShare) != 2 {
			return podShares, false
		}
		share, err := strconv.ParseFloat(podNameToShare[1], 64)
		if err != nil {
			return podShares, false
		}
		podShares[podNameToShare[0]] = fmt.Sprintf("%d", int64(share))
	}

	return podShares, true
}

func parseLBWeights(msg string) (map[string]float64, bool) {

	// example message to parse: "updateLBWeights svcA:45.5|69.22 svcB:54.7|44.1"

	weights := make(map[string]float64)
	podStrs := strings.Split(msg, " ")[1:]
	for _, podStr := range podStrs {
		podNameToShare := strings.Split(podStr, ":")
		if len(podNameToShare) != 2 {
			return weights, false
		}
		share, err := strconv.ParseFloat(podNameToShare[1], 64)
		if err != nil {
			return weights, false
		}
		weights[podNameToShare[0]] = share
	}

	return weights, true
}

func getOSFile(readPath string) (string, error) {

	// Reliable, but really really slow.
	out, err := exec.Command("cat", readPath).Output()
	return string(out), err

	// file, err := os.Open(readPath)
	// if err != nil {
	// 	fmt.Printf("Error opening file: %v\n", err)
	// 	return "", err
	// }
	// defer file.Close()

	// readBuf := make([]byte, 4096)
	// readFile := file

	// // Seek should tell us the new offset (0) and no err.
	// bytesRead := 0
	// _, err = readFile.Seek(0, 0)

	// // Loop until N > 0 AND err != EOF && err != timeout.
	// if err == nil {
	// 	n := 0
	// 	for {
	// 		n, err = readFile.Read(readBuf)
	// 		bytesRead += n
	// 		if os.IsTimeout(err) {
	// 			// bail out.
	// 			bytesRead = 0
	// 			break
	// 		}
	// 		if err == io.EOF {
	// 			// Success!
	// 			break
	// 		}
	// 		// Any other err means 'keep trying to read.'
	// 	}
	// }

	// return string(readBuf), err
}

func getCPUUtilizations(podUIDs map[string]string) string {

	response := "utils:"

	initialCPUUtils := make(map[string]int64)
	finalCPUUtils := make(map[string]int64)

	for podName, uid := range podUIDs {
		initialCPUUtils[podName] = getPodCPUUtil(uid)
	}
	intialTime := time.Now().UnixNano()

	time.Sleep(CPU_UTILIZATION_INTERVAL_MS * time.Millisecond)

	for podName, uid := range podUIDs {
		finalCPUUtils[podName] = getPodCPUUtil(uid)
	}
	timeElapsed := time.Now().UnixNano() - intialTime

	for podName := range podUIDs {
		response += fmt.Sprintf(" %s:%f",
			podName,
			(float64(finalCPUUtils[podName]-initialCPUUtils[podName])/
				float64(timeElapsed))*100)
	}

	return response
}

// // pathExists checks if a given path exists and is either a file or a directory.
// func pathExists(path string) bool {
// 	_, err := os.Stat(path)
// 	if os.IsNotExist(err) {
// 		return false
// 	}
// 	return err == nil
// }

// func getPodcgroupFilePath(uid string) string {
// 	qosClass := []string{"burstable/", "besteffort/", ""}
// 	for _, class := range qosClass {
// 		fileName := "/sys/fs/cgroup/cpu/kubepods/" + class + "pod" + uid
// 		if pathExists(fileName) {
// 			return fileName
// 		} else {
// 			slog.Warn("No cgroup path found: " + fileName)
// 		}
// 	}
// 	slog.Warn("No cgroup file found for pod: " + uid)
// 	return "/sys/fs/cgroup/cpu/kubepods/" + qosClass[0] + "pod" + uid
// }

func getPodCPUUtil(uid string) int64 {
	// get the CPU utilization of the pod
	// return the CPU utilization

	// read the file and return the value
	fileName := "/host/sys/fs/cgroup/cpu/kubepods/" + uid + "/cpuacct.usage"

	cpuUtil, err := getOSFile(fileName)
	if err != nil {
		slog.Warn(err.Error())
		return -1
	}

	slog.Info("CPU Utilization [" + uid + "]: \"" + cpuUtil + "\"")

	cpuUtilStr := strings.Trim(cpuUtil, "\n")

	cpuUtilInt64, err := strconv.ParseInt(cpuUtilStr, 10, 64)
	if err != nil {
		slog.Warn(err.Error())
		return -1
	}

	return cpuUtilInt64
}

func sendSuccessOrFailResponse(connection net.Conn, ok bool) {
	if ok {
		sendMsgToConnection(connection, "Success")
	} else {
		sendMsgToConnection(connection, "Failure")
	}
}

func readMsgFromConnection(connection net.Conn) (string, error) {
	buffer := make([]byte, 4096)
	mLen, err := connection.Read(buffer)
	return string(buffer[:mLen]), err
}

func sendMsgToConnection(connection net.Conn, msg string) {
	_, err := connection.Write([]byte(msg))
	if err != nil {
		fmt.Println("Error writing:", err.Error())
	} else {
		slog.Info("Sent: " + msg)
	}
}
