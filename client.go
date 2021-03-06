package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

func (c *Client) setNick(nick string) {
	//Set up new nick
	oldNick := c.nick
	oldKey := c.key
	c.nick = nick
	c.key = strings.ToLower(c.nick)

	delete(c.server.clientMap, oldKey)
	c.server.clientMap[c.key] = c

	//Update the relevant channels and notify everyone who can see us about our
	//nick change
	c.reply(rplNickChange, oldNick, c.nick)
	visited := make(map[*Client]struct{}, 100)
	for _, channel := range c.channelMap {
		delete(channel.clientMap, oldKey)

		for _, client := range channel.clientMap {
			if _, skip := visited[client]; skip {
				continue
			}
			client.reply(rplNickChange, oldNick, c.nick)
			visited[client] = struct{}{}
		}

		//Insert the new nick after iterating through channel.clientMap to avoid
		//sending a duplicate message to ourselves
		channel.clientMap[c.key] = c

		channel.modeMap[c.key] = channel.modeMap[oldKey]
		delete(channel.modeMap, oldKey)
	}
}

func (c *Client) joinChannel(channelName string) {
	newChannel := false

	channelKey := strings.ToLower(channelName)
	channel, exists := c.server.channelMap[channelKey]
	if exists == false {
		mode := ChannelMode{secret: true,
			topicLocked: true,
			noExternal:  true}
		channel = &Channel{name: channelName,
			topic:     "",
			clientMap: make(map[string]*Client),
			modeMap:   make(map[string]*ClientMode),
			mode:      mode}
		c.server.channelMap[channelKey] = channel
		newChannel = true
	}

	if _, inChannel := channel.clientMap[c.key]; inChannel {
		//Client is already in the channel, do nothing
		return
	}

	mode := new(ClientMode)
	if newChannel {
		//If they created the channel, make them op
		mode.operator = true
	}

	channel.clientMap[c.key] = c
	channel.modeMap[c.key] = mode
	c.channelMap[channelKey] = channel

	for _, client := range channel.clientMap {
		client.reply(rplJoin, c.nick, channel.name)
	}

	if channel.topic != "" {
		c.reply(rplTopic, channel.name, channel.topic)
	} else {
		c.reply(rplNoTopic, channel.name)
	}

	//The capacity sets the max number of nicks to send per message
	nicks := make([]string, 0, 128)

	for _, client := range channel.clientMap {
		prefix := ""

		if mode, exists := channel.modeMap[client.key]; exists {
			prefix = mode.Prefix()
		}

		if len(nicks) >= cap(nicks) {
			c.reply(rplNames, channelName, strings.Join(nicks, " "))
			nicks = nicks[:0]
		}

		nicks = append(nicks, fmt.Sprintf("%s%s", prefix, client.nick))
	}

	if len(nicks) > 0 {
		c.reply(rplNames, channelName, strings.Join(nicks, " "))
	}

	c.reply(rplEndOfNames, channelName)
}

func (c *Client) partChannel(channelName, reason string) {
	channelKey := strings.ToLower(channelName)
	channel, exists := c.server.channelMap[channelKey]
	if exists == false {
		return
	}

	if _, inChannel := channel.clientMap[c.key]; inChannel == false {
		//Client isn't in this channel, do nothing
		return
	}

	//Notify clients of the part
	for _, client := range channel.clientMap {
		client.reply(rplPart, c.nick, channel.name, reason)
	}

	delete(c.channelMap, channelKey)
	delete(channel.modeMap, c.key)
	delete(channel.clientMap, c.key)

	if len(channel.clientMap) == 0 {
		delete(c.server.channelMap, channelKey)
	}
}

func (c *Client) disconnect() {
	c.connected = false
	c.signalChan <- signalStop
}

//Send a reply to a user with the code specified
func (c *Client) reply(code replyCode, args ...string) {
	if c.connected == false {
		return
	}

	switch code {
	case rplWelcome:
		c.outputChan <- fmt.Sprintf(":%s 001 %s :Welcome to %s", c.server.name, c.nick, c.server.name)
	case rplJoin:
		c.outputChan <- fmt.Sprintf(":%s JOIN %s", args[0], args[1])
	case rplPart:
		c.outputChan <- fmt.Sprintf(":%s PART %s %s", args[0], args[1], args[2])
	case rplTopic:
		c.outputChan <- fmt.Sprintf(":%s 332 %s %s :%s", c.server.name, c.nick, args[0], args[1])
	case rplNoTopic:
		c.outputChan <- fmt.Sprintf(":%s 331 %s %s :No topic is set", c.server.name, c.nick, args[0])
	case rplNames:
		c.outputChan <- fmt.Sprintf(":%s 353 %s = %s :%s", c.server.name, c.nick, args[0], args[1])
	case rplEndOfNames:
		c.outputChan <- fmt.Sprintf(":%s 366 %s %s :End of NAMES list", c.server.name, c.nick, args[0])
	case rplNickChange:
		c.outputChan <- fmt.Sprintf(":%s NICK %s", args[0], args[1])
	case rplKill:
		c.outputChan <- fmt.Sprintf(":%s KILL %s A %s", args[0], c.nick, args[1])
	case rplMsg:
		c.outputChan <- fmt.Sprintf(":%s PRIVMSG %s %s", args[0], args[1], args[2])
	case rplList:
		c.outputChan <- fmt.Sprintf(":%s 322 %s %s", c.server.name, c.nick, args[0])
	case rplListEnd:
		c.outputChan <- fmt.Sprintf(":%s 323 %s", c.server.name, c.nick)
	case rplOper:
		c.outputChan <- fmt.Sprintf(":%s 381 %s :You are now an operator", c.server.name, c.nick)
	case rplChannelModeIs:
		c.outputChan <- fmt.Sprintf(":%s 324 %s %s %s %s", c.server.name, c.nick, args[0], args[1], args[2])
	case rplKick:
		c.outputChan <- fmt.Sprintf(":%s KICK %s %s %s", args[0], args[1], args[2], args[3])
	case rplInfo:
		c.outputChan <- fmt.Sprintf(":%s 371 %s :%s", c.server.name, c.nick, args[0])
	case rplVersion:
		c.outputChan <- fmt.Sprintf(":%s 351 %s %s", c.server.name, c.nick, args[0])
	case rplMOTDStart:
		c.outputChan <- fmt.Sprintf(":%s 375 %s :- Message of the day - ", c.server.name, c.nick)
	case rplMOTD:
		c.outputChan <- fmt.Sprintf(":%s 372 %s :- %s", c.server.name, c.nick, args[0])
	case rplEndOfMOTD:
		c.outputChan <- fmt.Sprintf(":%s 376 %s :End of MOTD Command", c.server.name, c.nick)
	case rplPong:
		c.outputChan <- fmt.Sprintf(":%s PONG %s %s", c.server.name, c.nick, c.server.name)
	case errMoreArgs:
		c.outputChan <- fmt.Sprintf(":%s 461 %s :Not enough params", c.server.name, c.nick)
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
	case errPassword:
		c.outputChan <- fmt.Sprintf(":%s 464 %s :Error, password incorrect", c.server.name, c.nick)
	case errNoPriv:
		c.outputChan <- fmt.Sprintf(":%s 481 %s :Permission denied", c.server.name, c.nick)
	case errCannotSend:
		c.outputChan <- fmt.Sprintf(":%s 404 %s %s :Cannot send to channel", c.server.name, c.nick, args[0])
	}
}

func (c *Client) clientThread() {
	readSignalChan := make(chan signalCode, 3)
	writeSignalChan := make(chan signalCode, 3)
	writeChan := make(chan string, 100)

	c.server.eventChan <- Event{client: c, event: connected}

	go c.readThread(readSignalChan)
	go c.writeThread(writeSignalChan, writeChan)

	defer func() {
		//Part from all channels
		for channelName := range c.channelMap {
			c.partChannel(channelName, "Disconnecting")
		}

		delete(c.server.clientMap, c.key)

		c.connection.Close()
	}()

	for {
		select {
		case signal := <-c.signalChan:
			if signal == signalStop {
				readSignalChan <- signalStop
				writeSignalChan <- signalStop
				return
			}
		case line := <-c.outputChan:
			select {
			case writeChan <- line:
				continue
			default:
				c.disconnect()
			}
		}
	}

}

func (c *Client) readThread(signalChan chan signalCode) {
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
					c.disconnect()
					return
				}
				continue
			}

			rawLines := buf[:ln]
			rawLines = bytes.Replace(rawLines, []byte("\r\n"), []byte("\n"), -1)
			rawLines = bytes.Replace(rawLines, []byte("\r"), []byte("\n"), -1)
			lines := bytes.Split(rawLines, []byte("\n"))
			for _, line := range lines {
				if len(line) > 0 {
					c.server.eventChan <- Event{client: c, event: command, input: string(line)}
				}
			}
		}
	}
}

func (c *Client) writeThread(signalChan chan signalCode, outputChan chan string) {
	for {
		select {
		case signal := <-signalChan:
			if signal == signalStop {
				return
			}
		case output := <-outputChan:
			c.connection.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if _, err := fmt.Fprintf(c.connection, "%s\r\n", output); err != nil {
				c.disconnect()
				return
			}
		}
	}
}
