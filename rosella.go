package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Server struct {
	eventChan  chan Event
	running    bool
	name       string
	clientMap  map[string]*Client  //Map of nicks → clients
	channelMap map[string]*Channel //Map of channel names → channels
}

type Client struct {
	server     *Server
	connection net.Conn
	signalChan chan int
	outputChan chan string
	nick       string
	registered bool
	connected  bool
	channelMap map[string]*Channel
}

type Channel struct {
	name      string
	topic     string
	clientMap map[string]*Client
}

type Event struct {
	client *Client
	input  string
}

const (
	signalStop int = iota
)

const (
	rplWelcome int = iota
	rplJoin
	rplPart
	rplTopic
	rplNoTopic
	rplNames
	rplNickChange
	rplKill
	rplMsg
	rplList
	errMoreArgs
	errNoNick
	errInvalidNick
	errNickInUse
	errAlreadyReg
	errNoSuchNick
	errUnknownCommand
	errNotReg
)

var (
	nickRegexp    = regexp.MustCompile(`^[a-zA-Z\[\]_^{|}][a-zA-Z0-9\[\]_^{|}]*$`)
	channelRegexp = regexp.MustCompile(`^#[a-z0-9_\-]+$`)
)

func NewServer() *Server {
	return &Server{eventChan: make(chan Event),
		name:       "rosella",
		clientMap:  make(map[string]*Client),
		channelMap: make(map[string]*Channel)}
}

func (s *Server) Run() {
	go func() {
		for {
			s.handleEvent(<-s.eventChan)
		}
	}()
}

func (s *Server) HandleConnection(conn net.Conn) {

	client := &Client{server: s,
		connection: conn,
		outputChan: make(chan string),
		signalChan: make(chan int, 3),
		channelMap: make(map[string]*Channel),
		connected:  true}

	go client.clientThread()
}

func (s *Server) handleEvent(e Event) {
	fields := strings.Fields(e.input)

	if len(fields) < 1 {
		return
	}

	if strings.HasPrefix(fields[0], ":") {
		fields = fields[1:]
	}

	command := strings.ToUpper(fields[0])
	args := fields[1:]

	switch {
	case command == "NICK":
		if len(args) < 1 {
			e.client.reply(errNoNick)
			return
		}

		newNick := args[0]

		//Check newNick is of valid formatting (regex)
		if nickRegexp.MatchString(newNick) == false {
			e.client.reply(errInvalidNick, newNick)
			return
		}

		if _, exists := s.clientMap[newNick]; exists {
			e.client.reply(errNickInUse, newNick)
			return
		}

		//Protect the server name from being used
		if newNick == s.name {
			e.client.reply(errNickInUse, newNick)
			return
		}

		e.client.setNick(newNick)

	case command == "USER":
		if e.client.nick == "" {
			e.client.reply(rplKill, "Your nickname is already being used")
			e.client.disconnect()
		} else {
			e.client.reply(rplWelcome)
			e.client.registered = true
		}

	case command == "JOIN":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		if len(args) < 1 {
			e.client.reply(errMoreArgs)
			return
		}

		if args[0] == "0" {
			//Quit all channels
			for channel := range e.client.channelMap {
				s.partChannel(e.client, channel)
			}
			return
		}

		channels := strings.Split(args[0], ",")
		for _, channel := range channels {
			//Join the channel if it's valid
			if channelRegexp.Match([]byte(channel)) {
				s.joinChannel(e.client, channel)
			}
		}

	case command == "PART":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		if len(args) < 1 {
			e.client.reply(errMoreArgs)
			return
		}

		channels := strings.Split(args[0], ",")
		for _, channel := range channels {
			//Part the channel if it's valid
			if channelRegexp.Match([]byte(channel)) {
				s.partChannel(e.client, channel)
			}
		}

	case command == "PRIVMSG":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		if len(args) < 2 {
			e.client.reply(errMoreArgs)
			return
		}

		message := strings.Join(args[1:], " ")

		channel, chanExists := s.channelMap[args[0]]
		client, clientExists := s.clientMap[args[0]]

		if chanExists {
			for _, c := range channel.clientMap {
				if c != e.client {
					c.reply(rplMsg, e.client.nick, args[0], message)
				}
			}
		} else if clientExists {
			client.reply(rplMsg, e.client.nick, client.nick, message)
		} else {
			e.client.reply(errNoSuchNick, args[0])
		}

	case command == "QUIT":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		//Stop the client, which will auto part channels and quit
		e.client.disconnect()

	case command == "TOPIC":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		if len(args) < 1 {
			e.client.reply(errMoreArgs)
			return
		}

		channel, exists := s.channelMap[args[0]]
		if exists == false {
			e.client.reply(errNoSuchNick, args[0])
			return
		}

		channelName := args[0]

		if len(args) == 1 {
			e.client.reply(rplTopic, channelName, channel.topic)
			return
		}

		if args[1] == ":" {
			channel.topic = ""
			for _, client := range channel.clientMap {
				client.reply(rplNoTopic, channelName)
			}
		} else {
			topic := strings.Join(args[1:], " ")
			topic = strings.TrimPrefix(topic, ":")
			channel.topic = topic

			for _, client := range channel.clientMap {
				client.reply(rplTopic, channelName, channel.topic)
			}
		}

	case command == "LIST":
		if e.client.registered == false {
			e.client.reply(errNotReg)
			return
		}

		if len(args) == 0 {
			chanList := make([]string, 0, len(s.channelMap))

			for channelName, channel := range s.channelMap {
				listItem := fmt.Sprintf("%s %d :%s", channelName, len(channel.clientMap), channel.topic)
				chanList = append(chanList, listItem)
			}

			e.client.reply(rplList, chanList...)

		} else {
			channels := strings.Split(args[0], ",")
			chanList := make([]string, 0, len(channels))

			for _, channelName := range channels {
				if channel, exists := s.channelMap[channelName]; exists {
					listItem := fmt.Sprintf("%s %d :%s", channelName, len(channel.clientMap), channel.topic)
					chanList = append(chanList, listItem)
				}
			}

			e.client.reply(rplList, chanList...)
		}

	default:
		e.client.reply(errUnknownCommand, command)
	}
}

func (s *Server) joinChannel(client *Client, channelName string) {
	channel, exists := s.channelMap[channelName]
	if exists == false {
		channel = &Channel{name: channelName,
			topic:     "",
			clientMap: make(map[string]*Client)}
		s.channelMap[channelName] = channel
	}

	channel.clientMap[client.nick] = client
	client.channelMap[channelName] = channel

	for _, c := range channel.clientMap {
		c.reply(rplJoin, client.nick, channelName)
	}

	if channel.topic != "" {
		client.reply(rplTopic, channelName, channel.topic)
	} else {
		client.reply(rplNoTopic, channelName)
	}

	nicks := make([]string, 0, 100)
	for nick := range channel.clientMap {
		nicks = append(nicks, nick)
	}

	client.reply(rplNames, channelName, strings.Join(nicks, " "))
}

func (s *Server) partChannel(client *Client, channelName string) {
	channel, exists := s.channelMap[channelName]
	if exists == false {
		return
	}

	//Notify clients of the part
	for _, c := range channel.clientMap {
		c.reply(rplPart, client.nick, channelName)
	}

	delete(channel.clientMap, client.nick)
	delete(client.channelMap, channelName)
}

func (c *Client) clientThread() {
	defer c.connection.Close()

	readSignalChan := make(chan int, 3)
	writeSignalChan := make(chan int, 3)
	writeChan := make(chan string, 100)

	go c.readThread(readSignalChan)
	go c.writeThread(writeSignalChan, writeChan)

	for {
		select {
		case signal := <-c.signalChan:
			//Do stuff
			if signal == signalStop {
				readSignalChan <- signalStop
				writeSignalChan <- signalStop
				break
			}
		case line := <-c.outputChan:
			select {
			case writeChan <- line:
				//It worked
			default:
				log.Printf("Dropped a line for client: %q", c.nick)
				//Do nothing, dropping the line
			}
		}
	}

	//Part from all channels
	for channelName := range c.channelMap {
		c.server.partChannel(c, channelName)
	}

	delete(c.server.clientMap, c.nick)

}

func (c *Client) readThread(signalChan chan int) {
	for {
		select {
		case signal := <-signalChan:
			if signal == signalStop {
				return
			}
		default:
			c.connection.SetReadDeadline(time.Now().Add(time.Second * 3))
			buf := make([]byte, 512)
			ln, err := c.connection.Read(buf)
			if err != nil {
				if err == io.EOF {
					//They must have dc'd
					c.signalChan <- signalStop
					return
				}
				continue
			}

			rawLines := buf[:ln]
			lines := bytes.Split(rawLines, []byte("\r\n"))
			for _, line := range lines {
				if len(line) > 0 {
					c.server.eventChan <- Event{client: c, input: string(line)}
				}
			}
		}
	}
}

func (c *Client) writeThread(signalChan chan int, outputChan chan string) {
	for {
		select {
		case signal := <-signalChan:
			if signal == signalStop {
				return
			}
		case output := <-outputChan:
			line := []byte(fmt.Sprintf("%s\r\n", output))

			c.connection.SetWriteDeadline(time.Now().Add(time.Second * 30))
			_, err := c.connection.Write(line)
			if err != nil {
				log.Printf("Write err: %q", err.Error())
				c.signalChan <- signalStop
				return
			}
		}
	}
}

func (c *Client) disconnect() {
	c.connected = false
	c.signalChan <- signalStop
}

//Send a reply to a user with the code specified
func (c *Client) reply(code int, args ...string) {
	if c.connected == false {
		return
	}

	switch code {
	case rplWelcome:
		c.outputChan <- fmt.Sprintf(":%s 001 %s :Welcome to %s", c.server.name, c.nick, c.server.name)
	case rplJoin:
		c.outputChan <- fmt.Sprintf(":%s JOIN %s", args[0], args[1])
	case rplPart:
		c.outputChan <- fmt.Sprintf(":%s PART %s", args[0], args[1])
	case rplTopic:
		c.outputChan <- fmt.Sprintf(":%s 332 %s %s :%s", c.server.name, c.nick, args[0], args[1])
	case rplNoTopic:
		c.outputChan <- fmt.Sprintf(":%s 331 %s %s :No topic is set", c.server.name, c.nick, args[0])
	case rplNames:
		//TODO: break long lists up into multiple messages
		c.outputChan <- fmt.Sprintf(":%s 353 %s = %s :%s", c.server.name, c.nick, args[0], args[1])
		c.outputChan <- fmt.Sprintf(":%s 366 %s", c.server.name, c.nick)
	case rplNickChange:
		c.outputChan <- fmt.Sprintf(":%s NICK %s", args[0], args[1])
	case rplKill:
		c.outputChan <- fmt.Sprintf(":%s KILL %s A A %s", c.server.name, c.nick, args[0])
	case rplMsg:
		c.outputChan <- fmt.Sprintf(":%s PRIVMSG %s %s", args[0], args[1], args[2])
	case rplList:
		c.outputChan <- fmt.Sprintf(":%s 321 %s", c.server.name, c.nick)
		for _, listItem := range args {
			c.outputChan <- fmt.Sprintf(":%s 322 %s %s", c.server.name, c.nick, listItem)
		}
		c.outputChan <- fmt.Sprintf(":%s 323 %s", c.server.name, c.nick)
	case errMoreArgs:
		c.outputChan <- fmt.Sprintf(":%s 461 %s %s :Not enough params", c.server.name, c.nick, args[0])
	case errNoNick:
		c.outputChan <- fmt.Sprintf(":%s 431 %s :No nickname given", c.server.name, c.nick)
	case errInvalidNick:
		c.outputChan <- fmt.Sprintf(":%s 432 %s %s :Erronenous nickname", c.server.name, c.nick, args[0])
	case errNickInUse:
		c.outputChan <- fmt.Sprintf(":%s 433 %s %s :Nick already in use", c.server.name, c.nick, args[0])
	case errAlreadyReg:
		c.outputChan <- fmt.Sprintf(":%s 462 :You need a valid nick first", c.server.name)
	case errNoSuchNick:
		c.outputChan <- fmt.Sprintf(":%s 401 %s %s :No such nick/channel", c.server.name, c.nick, args[0])
	case errUnknownCommand:
		c.outputChan <- fmt.Sprintf(":%s 421 %s %s :Unknown command", c.server.name, c.nick, args[0])
	case errNotReg:
		c.outputChan <- fmt.Sprintf(":%s 451 :You have not registered", c.server.name)
	}
}

func (c *Client) setNick(nick string) {
	if c.nick != "" {
		delete(c.server.clientMap, c.nick)
		for _, channel := range c.channelMap {
			delete(channel.clientMap, c.nick)
		}
	}

	//Set up new nick
	oldNick := c.nick
	c.nick = nick
	c.server.clientMap[c.nick] = c

	clients := make([]string, 0, 100)

	for _, channel := range c.channelMap {
		channel.clientMap[c.nick] = c

		//Collect list of client nicks who can see us
		for client := range channel.clientMap {
			clients = append(clients, client)
		}
	}

	//By sorting the nicks and skipping duplicates we send each client one message
	sort.Strings(clients)
	prevNick := ""
	for _, nick := range clients {
		if nick == prevNick {
			continue
		}
		prevNick = nick

		client, exists := c.server.clientMap[nick]
		if exists {
			client.reply(rplNickChange, oldNick, c.nick)
		}
	}
}
