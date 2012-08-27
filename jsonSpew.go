package main

import (
	"code.google.com/p/go.net/websocket"
	"encoding/json"
	"errors"
	"exp/inotify"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf8"
)

var channelBuffer = 1024
var socketBuffer = 4096
var connections = make([]*websocket.Conn, 0)
var web = make(chan *websocket.Conn, channelBuffer)
var tcp = make(chan Message, channelBuffer)
var websocketHandler = websocket.Handler(WebsocketServer)
var myLog *log.Logger = log.New(os.Stdout, "", log.Ldate+log.Lmicroseconds)
var watcher *inotify.Watcher
var watchList string

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	var version = "1.0.a"

	var ff = flag.String("fileList", "/", "File list. : seperated")
	var hf = flag.Int("httpPort", 8080, "HTTP Port")
	var sf = flag.Bool("serveLocalhost", false, "Serve as localhost")
	var tf = flag.Int("tcpPort", 0, "TCP port")
	var df = flag.Int("tcpDelim", 10, "TCP message delimiter")
	var wf = flag.String("watchList", "./", "Watch list. : seperated")
	var _ = flag.String("version", version, "Version")

	var fileList string
	var serveLocalhost bool
	var httpPort int
	var tcpPort int
	var tcpDelimiter string
	var err error

	flag.Parse()

	watchList = *wf
	fileList = *ff
	serveLocalhost = *sf
	httpPort = *hf
	tcpPort = *tf
	tcpDelimiter = EncChar(*df)

	myLog.Printf("jsonSpew %v\n", version)

	if watcher, err = inotify.NewWatcher(); err != nil {
		myLog.Fatalln(err)
	}

	for _, f := range strings.Split(fileList, ":") {
		http.HandleFunc(f, FileHandler)
		myLog.Printf("Registered %v\n", f)
	}

	for _, f := range strings.Split(watchList, ":") {
		if err := watcher.AddWatch(f, inotify.IN_CLOSE); err != nil {
			myLog.Println(err)
		}
		myLog.Printf("Watching:%v", f)
	}

	go channelSelect()

	http.Handle("/websocket", websocketHandler)

	hostname, _ := os.Hostname()
	host, _ := net.LookupHost(hostname)

	if serveLocalhost {
		host = append(host, "127.0.0.1")
	}

	if tcpPort != 0 {
		myLog.Printf("Serving tcp on %v %v", host, tcpPort)
		go ServeTCPChannel(fmt.Sprintf("%v:%v", host, tcpPort), tcpDelimiter, tcp)
	}

	myLog.Printf("Serving http on %v:%v\n", host, httpPort)
	if err := http.ListenAndServe(fmt.Sprintf("%v:%v", host, httpPort), nil); err != nil {
		panic(err)
	}

}

func TouchWatches() {
	for _, f := range strings.Split(watchList, ":") {
		cmd := exec.Command("touch", f)
		cmd.Run()
	}
}

func channelSelect() {
	var m map[string]interface{}

	for {
		select {

		case con := <-web:
			connections = append(connections, con)
			myLog.Printf("Websocket:%v %v", con.LocalAddr().String(), con.RemoteAddr().String())
			// touch the watches for a initial view
			// all clients will get it, but hey!
			TouchWatches()

		case msg := <-tcp:
			if msg.Msg != "" {
				if e := json.Unmarshal([]byte(msg.Msg), &m); e == nil {
					myLog.Printf("Tcp:%v", msg.RemoteAddr)
					toConnections(m)
				} else {
					myLog.Println(e)
				}
			}

		case ev := <-watcher.Event:
			if ev.Mask&inotify.IN_ISDIR != inotify.IN_ISDIR {

				if f, e := os.Open(ev.Name); e == nil {
					d := json.NewDecoder(f)

					if e = d.Decode(&m); e == nil {
						myLog.Printf("Event:%v", ev)
						toConnections(m)
					} else {
						myLog.Println(e)
					}

				} else {
					myLog.Println(e)
				}
			}

		case err := <-watcher.Error:
			myLog.Printf("Error:%v", err)
		}

	}
}

func toConnections(m map[string]interface{}) {
	b, _ := json.Marshal(m)

	for i, ws := range connections {
		if ws != nil {
			if _, err := ws.Write(b); err != nil {
				myLog.Println(err)
				connections[i] = nil
			}
		}
	}
}

func WebsocketServer(ws *websocket.Conn) {
	web <- ws
	select {}
}

func FileHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, fmt.Sprintf("./%v", r.URL.Path[1:]))
}

type Message struct {
	Msg        string
	RemoteAddr net.Addr
	RespCh     chan string
}

func EncChar(i int) string {
	b := make([]byte, 1)
	utf8.EncodeRune(b, rune(i))
	return string(b[0])
}

func ServeTCPChannel(addr, delimiter string, toClient chan Message) error {
	a, err := net.ResolveTCPAddr("tcp4", addr)
	ln, err := net.ListenTCP("tcp4", a)

	if err != nil {
		myLog.Println(err)
		return err
	}

	for {
		if conn, err := ln.AcceptTCP(); err == nil {
			myLog.Printf("Connection:%v ", conn.RemoteAddr())

			go func() {
				var toTCP chan string = make(chan string, channelBuffer)
				var fromTCP chan Message = make(chan Message, channelBuffer)

				go func() {
					for {
						if tcp, ok := <-fromTCP; !ok {
							return
						} else {
							tcp.RespCh = toTCP
							toClient <- tcp
						}
					}

				}()

				// Dont actually need this write at the moment, but i might want to reply at some point.
				// The channel to reply on will be Message.RespCh as assigned above
				go WriteTCPChannel(conn, delimiter, toTCP)
				ReadTCPChannel(conn, delimiter, fromTCP)
			}()
		} else {
			myLog.Println(err)
			break
		}

	}

	return nil
}

func ReadTCPChannel(conn *net.TCPConn, delimiter string, fromSocket chan Message) error {
	var message string
	buffer := make([]byte, socketBuffer)

	for {
		if n, err := conn.Read(buffer); err != nil || n == 0 {
			if n == 0 && err == nil {
				err = errors.New("No bytes")
			}
			myLog.Printf("Closing:%v %v\n", conn.RemoteAddr(), err)
			conn.Close()
			return err
		} else {

			message += string(buffer[0:n])

			m := strings.Split(message, delimiter)
			for i, entry := range m {
				if i < len(m)-1 {
					fromSocket <- Message{Msg: entry, RemoteAddr: conn.RemoteAddr()}
				} else {
					// overflow
					message = entry
				}
			}
		}

	}
	return nil
}

func WriteTCPChannel(conn *net.TCPConn, delimiter string, toSocket chan string) error {
	for {
		if message, ok := <-toSocket; !ok {
			err := errors.New("Error on socket channel")
			myLog.Printf("Closing:%v %v\n", conn.RemoteAddr(), err)
			conn.Close()
			return err
		} else {
			message += delimiter
			outBytes := []byte(message)
			if _, err := conn.Write(outBytes); err != nil {
				myLog.Printf("Closing:%v %v\n", conn.RemoteAddr(), err)
				conn.Close()
				return err
			}
		}
	}
	return nil
}
