package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
)

var SocksProxyAddr string

type localForwardChannelData struct {
	DestAddr string
	DestPort uint32

	OriginAddr string
	OriginPort uint32
}

func listKeys(dirPath string) (result []string, err error) {
	err = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, "_key") {
			result = append(result, path)
		}
		return nil
	})
	return
}

func isLocalIP(dhost string) bool{
	ip := net.ParseIP(dhost)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsPrivate()
}

func directTCPIPClosure(rdb *redis.Client) ssh.ChannelHandler {
	return func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
		d := localForwardChannelData{}
		if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
			newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
			return
		}

		ipAddr, err := net.ResolveIPAddr("ip4", d.DestAddr)
		if err != nil {
			ipAddr, err = net.ResolveIPAddr("ip6", d.DestAddr)
			if err != nil {
				newChan.Reject(gossh.Prohibited, "cannot resolve the said address: "+d.DestAddr)
				return
			}
		}

		dest := ipAddr.String()

		if srv.LocalPortForwardingCallback == nil || !srv.LocalPortForwardingCallback(ctx, dest, d.DestPort) {
			newChan.Reject(gossh.Prohibited, "illegal address")
			return
		}
		
		dest = net.JoinHostPort(dest, strconv.FormatInt(int64(d.DestPort), 10))

		var dialer net.Dialer
		var dconn net.Conn

		if len(SocksProxyAddr) != 0 && !isLocalIP(dest) {
			pDialer, err := proxy.SOCKS5("tcp", SocksProxyAddr, nil, proxy.Direct)
			if err != nil {
				newChan.Reject(gossh.ConnectionFailed, err.Error())
				return
			}
			dconn, err = pDialer.Dial("tcp", dest)
			if err != nil {
				newChan.Reject(gossh.ConnectionFailed, err.Error())
				return
			}
		} else {
			dconn, err = dialer.DialContext(ctx, "tcp", dest)
			if err != nil {
				newChan.Reject(gossh.ConnectionFailed, err.Error())
				return
			}
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			dconn.Close()
			return
		}
		go gossh.DiscardRequests(reqs)

		go func() {
			defer ch.Close()
			defer dconn.Close()
			result, _ := io.Copy(ch, dconn)
			userID := ctx.User()
			rdb.HIncrBy(context.Background(), "ssh-server:users-usage", userID, result)
		}()
		go func() {
			defer ch.Close()
			defer dconn.Close()
			result, _ := io.Copy(dconn, ch)
			userID := ctx.User()
			rdb.HIncrBy(context.Background(), "ssh-server:users-usage", userID, result)
		}()
	}
}

func getAllEnvHostKeys() ([]gossh.Signer, error) {
	// Create a slice to store the parsed private keys
	keys := []gossh.Signer{}

	// Loop through each key from HOST_KEY_1 to HOST_KEY_4
	for i := 1; i <= 4; i++ {
		// Construct the environment variable name
		envVar := fmt.Sprintf("HOST_KEY_%d", i)

		// Get the Base64 encoded value from the environment
		base64Encoded := os.Getenv(envVar)
		if base64Encoded == "" {
			log.Printf("Environment variable %s is not set", envVar)
			continue
		}

		// Decode the Base64 encoded value
		decoded, err := base64.StdEncoding.DecodeString(base64Encoded)
		if err != nil {
			log.Printf("Failed to decode %s: %v", envVar, err)
			continue
		}

		// Parse the decoded private key
		key, err := gossh.ParsePrivateKey(decoded)
		if err != nil {
			log.Printf("Failed to parse private key from %s: %v", envVar, err)
			continue
		}

		// Append the parsed key to the slice
		keys = append(keys, key)
	}

	return keys, nil
}
func parseHostKeyFile(keyFile string) (ssh.Signer, error) {
	file, err := os.Open(keyFile)
	if err != nil {
		return nil, err
	}

	keyBytes, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	key, err := gossh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, err
	}

	return key, nil
}

func extractNumbers(input string) ([]uint32, error) {
	elements := strings.Split(input, ",")
	numbers := make([]uint32, 0)
	if len(input) == 0 {
		return numbers, nil
	}

	for _, element := range elements {
		num, err := strconv.ParseUint(strings.TrimSpace(element), 10, 30) // limit to 30 bits just to be on the safe side
		if err != nil {
			return nil, err
		}
		numbers = append(numbers, uint32(num))
	}

	return numbers, nil
}

func containsNumber(list []uint32, number uint32) bool {
	for _, v := range list {
		if v == number {
			return true
		}
	}
	return false
}

func main() {
	var err error
	if len(os.Args) == 2 {
		err = godotenv.Load(os.Args[1])
	} else {
		err = godotenv.Load()
	}
	if err != nil {
		log.Fatalln(err)
	}

	redisUrl, ok := os.LookupEnv("REDIS_URL")
	if !ok {
		log.Fatalln("REDIS_URL not provided. Consider adding it to .env or the environment variables")
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if len(listenAddr) == 0 {
		listenAddr = ":2222"
	}

	SocksProxyAddr = os.Getenv("SOCKS_PROXY")
	whitelistString := os.Getenv("WHITELIST_PORTS")
	whitelistPorts, err := extractNumbers(whitelistString)

	if err != nil {
		log.Fatalln("Invalid WHITELIST_PORTS")
	}

	hostKeyPath := os.Getenv("HOST_KEY_PATH")
	if len(hostKeyPath) == 0 {
		hostKeyPath = "/root/etc/ssh/"
	}

	maxConnString := os.Getenv("MAX_CONNECTIONS")
	maxConns, err := strconv.ParseInt(maxConnString, 10, 32)
	if maxConns == 0 || len(maxConnString) == 0 || err != nil {
		log.Fatalln("Invalid MAX_CONNECTIONS parameter")
	}

	defaultVersionString, ok := os.LookupEnv("DEFAULT_SERVER_VERSION")
	if !ok {
		log.Fatalln("DEFAULT_SERVER_VERSION not provided. Aborting")
	}
	if !strings.HasPrefix(defaultVersionString, "SSH-2.0-") {
		log.Fatalln("DEFAULT_SERVER_VERSION should start with `SSH-2.0-`")
	}
	defaultVersionString = defaultVersionString[8:]
	copyVersionString := os.Getenv("COPY_SERVER_VERSION")
	shouldCopyVersionString := true
	if len(copyVersionString) == 0 || strings.ToLower(copyVersionString) == "disabled" {
		shouldCopyVersionString = false
	}

	opts, err := redis.ParseURL(redisUrl)
	if err != nil {
		log.Fatalln(err)
	}
	rdb := redis.NewClient(opts) // This is safe to use concurrently
	pingRes := rdb.Ping(context.Background())
	_, err = pingRes.Result()
	if err != nil {
		log.Fatalf("Could not reach the redis server. Aborting: %v", err)
	}
	rdb.Del(context.Background(), "ssh-server:connections")
	var userConnectionCountMutex sync.Mutex
	server := ssh.Server{
		LocalPortForwardingCallback: ssh.LocalPortForwardingCallback(func(ctx ssh.Context, dhost string, dport uint32) bool {
			//log.Printf("requesting %s", dhost)
			if !isLocalIP(dhost){return true}
			if containsNumber(whitelistPorts, dport) {
				return true
			}
			return false
		}),
		Addr: listenAddr,
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"direct-tcpip": directTCPIPClosure(rdb),
		},
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			//log.Printf("User -%s- with key -%s-", ctx.User(), string(gossh.MarshalAuthorizedKey(key)))
			if len(ctx.User()) != 36 { // it isn't a UUID
				return false
			}
			userId := ctx.User()
			userString := userId + "::" + string(gossh.MarshalAuthorizedKey(key))
			userString = strings.Trim(userString, "\n\t\r")
			result := rdb.SIsMember(ctx, "ssh-server:users", userString)
			res, err := result.Result()
			doneCh := ctx.Done()
			//log.Printf("UserString -%s- res -%s- err -%s-", userString,res,err)
			if err != nil || !res || doneCh == nil {
				//log.Printf("returning false 1")
				return false
			}
			userConnectionCountMutex.Lock()
			defer userConnectionCountMutex.Unlock()
			hget_res := rdb.HGet(ctx, "ssh-server:connections", userId)
			// It doesn't matter if we get an error (the key does not exist),
			// if there is something more serious it will be handled in HIncrBy
			connCntStr, _ := hget_res.Result()
			connCnt, err2 := strconv.ParseInt(connCntStr, 10, 32)
			if err2 == nil && connCnt >= maxConns {
				//log.Printf("returning false 2")
				//log.Printf("Client %s trying to have more than %d connections\n", userString, maxConns)
				return false // No duplicate connections
			}
			hincr_res := rdb.HIncrBy(ctx, "ssh-server:connections", userId, 1)
			if hincr_res.Err() != nil {
				//log.Printf("returning false 3 %s",hincr_res.Err())
				return false
			}
			go func() {
				<-doneCh
				//log.Printf("4---",userId)
				rdb.HIncrBy(context.Background(), "ssh-server:connections", userId, -1)
			}()
			//log.Printf("returning true ")
			return true
		},
		IdleTimeout: time.Minute * 1,
		MaxTimeout:  time.Hour * 6,
		Version:     defaultVersionString,
	}

	var versionStringMutex sync.Mutex // Not really used now, but can be helpful in the future
	go func() {
		if !shouldCopyVersionString {
			log.Println("Not copying the version string from another server")
			return
		}
		buf := make([]byte, 256)
		for {
			delayAmount := time.Hour * 1
			delayAmount += time.Millisecond * time.Duration(rand.Float32()*3600*1000)
			conn, err := net.Dial("tcp", copyVersionString)
			if err != nil {
				log.Printf("Could not copy the version string from another server: %v\n", err)
				time.Sleep(delayAmount)
				continue
			}
			n, err := conn.Read(buf)
			if err != nil || n == len(buf) {
				log.Printf("Invalid response from the to-be-copied ssh server, len=%d: %v\n", n, err)
				time.Sleep(delayAmount)
				conn.Close()
				continue
			}
			conn.Close()
			// Note! We should remove trailing zeros!
			resBuf := make([]byte, 0)
			for _, c := range buf {
				if c == 0 {
					break
				}
				resBuf = append(resBuf, c)
			}
			result := string(resBuf)
			result = strings.Trim(result, "\n\t\r")
			if !strings.HasPrefix(result, "SSH-2.0-") {
				log.Printf("The result from to-be-copied ssh server is invalid, does not start with `SSH-2.0-`")
				time.Sleep(delayAmount)
				continue
			}
			result = result[8:]
			versionStringMutex.Lock()
			server.Version = result
			versionStringMutex.Unlock()
			time.Sleep(delayAmount)
		}
	}()

	hostKeyFiles, err := listKeys(hostKeyPath)
	if err != nil {
		log.Fatalf("Could not get the host keys: %v\n", err)
	}
	for _, keyFile := range hostKeyFiles {
		hostKey, err := parseHostKeyFile(keyFile)
		if err != nil {
			log.Fatalf("Failed to parse host key file %s: %v", keyFile, err)
		}

		server.AddHostKey(hostKey)
	}
	envkeys, err := getAllEnvHostKeys()
	if err != nil {
		log.Fatalf("Failed to parse end keys  %v", err)
	}
	for _, hostKey := range envkeys {
		server.AddHostKey(hostKey)
	}

	time.Sleep(time.Second * 1) // Wait for the version string to settle in

	log.Printf("starting ssh-liberty-bridge on %s...\n", listenAddr)
	log.Fatal(server.ListenAndServe())
}
