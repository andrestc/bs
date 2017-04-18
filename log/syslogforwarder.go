// Copyright 2016 bs authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package log

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/tsuru/bs/bslog"
	"github.com/tsuru/bs/config"
)

const (
	udpMessageDefaultMTU = 1500
	udpHeaderSz          = 100 // Exagerated a bit due to possibility of ipv6 extensions, ipsec, etc.
)

type syslogBackend struct {
	syslogLocation   *time.Location
	syslogExtraStart []byte
	syslogExtraEnd   []byte
	msgChans         []chan<- LogMessage
	quitChans        []chan<- bool
	bufferPool       sync.Pool
	nextNotify       *time.Timer
}

type syslogForwarder struct {
	url          *url.URL
	bufferPool   *sync.Pool
	mtu          int
	messageLimit int
}

func (b *syslogBackend) initialize() error {
	extra := config.StringEnvOrDefault("", "LOG_SYSLOG_MESSAGE_EXTRA_START")
	if extra != "" {
		b.syslogExtraStart = []byte(os.ExpandEnv(extra) + " ")
	}
	extra = config.StringEnvOrDefault("", "LOG_SYSLOG_MESSAGE_EXTRA_END")
	if extra != "" {
		b.syslogExtraEnd = []byte(" " + os.ExpandEnv(extra))
	}
	bufferSize := config.IntEnvOrDefault(config.DefaultBufferSize, "LOG_SYSLOG_BUFFER_SIZE", "LOG_BUFFER_SIZE")
	forwardAddresses := config.StringsEnvOrDefault(nil, "LOG_SYSLOG_FORWARD_ADDRESSES", "SYSLOG_FORWARD_ADDRESSES")
	if len(forwardAddresses) == 0 {
		return nil
	}
	syslogTimezone := config.StringEnvOrDefault("", "LOG_SYSLOG_TIMEZONE", "SYSLOG_TIMEZONE")
	b.syslogLocation = time.Local
	if syslogTimezone != "" {
		tz, err := time.LoadLocation(syslogTimezone)
		if err == nil {
			b.syslogLocation = tz
		} else {
			bslog.Warnf("unable to parse syslog timezone format: %s", err)
		}
	}
	mtu := udpMessageDefaultMTU
	mtuInterface := config.StringEnvOrDefault("eth0", "LOG_SYSLOG_MTU_NETWORK_INTERFACE")
	if mtuInterface != "" {
		iface, err := net.InterfaceByName(mtuInterface)
		if err == nil && iface.MTU > 0 {
			mtu = iface.MTU
		} else {
			bslog.Warnf("unable to read mtu from interface, using default %d: %s", mtu, err)
		}
	}
	b.bufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 200)
		},
	}
	b.nextNotify = time.NewTimer(0)
	for _, addr := range forwardAddresses {
		forwardUrl, err := url.Parse(addr)
		if err != nil {
			return fmt.Errorf("unable to parse %q: %s", addr, err)
		}
		forwardChan, quitChan, err := processMessages(&syslogForwarder{
			url:        forwardUrl,
			bufferPool: &b.bufferPool,
			mtu:        mtu,
		}, bufferSize)
		if err != nil {
			return err
		}
		b.msgChans = append(b.msgChans, forwardChan)
		b.quitChans = append(b.quitChans, quitChan)
	}
	return nil
}

type bufferWithIdx struct {
	buffer     []byte
	headerIdx  int
	contentIdx int
}

func (b *syslogBackend) sendMessage(parts *rawLogParts, appName, processName, container string) {
	lenSyslogs := len(b.msgChans)
	if lenSyslogs == 0 {
		return
	}
	contID := parts.container
	if len(contID) > containerIDTrimSize {
		contID = contID[:containerIDTrimSize]
	}
	buffer := b.bufferPool.Get().([]byte)[:0]
	buffer = append(buffer, '<')
	buffer = append(buffer, parts.priority...)
	buffer = append(buffer, '>')
	buffer = append(buffer, parts.ts.In(b.syslogLocation).Format(time.Stamp)...)
	buffer = append(buffer, ' ')
	buffer = append(buffer, contID...)
	buffer = append(buffer, ' ')
	buffer = append(buffer, appName...)
	buffer = append(buffer, '[')
	buffer = append(buffer, processName...)
	buffer = append(buffer, ']', ':', ' ')
	buffer = append(buffer, b.syslogExtraStart...)
	headerIdx := len(buffer)
	buffer = append(buffer, parts.content...)
	contentIdx := len(buffer)
	buffer = append(buffer, b.syslogExtraEnd...)
	buffer = append(buffer, '\n')
	for i, ch := range b.msgChans {
		var chBuffer []byte
		if i == lenSyslogs-1 {
			chBuffer = buffer
		} else {
			chBuffer = b.bufferPool.Get().([]byte)[:0]
			chBuffer = append(chBuffer, buffer...)
		}
		select {
		case ch <- bufferWithIdx{
			buffer:     chBuffer,
			headerIdx:  headerIdx,
			contentIdx: contentIdx,
		}:
		default:
			select {
			case <-b.nextNotify.C:
				bslog.Errorf("Dropping log messages to syslog due to full channel buffer.")
				b.nextNotify.Reset(time.Minute)
			default:
			}
		}
	}
}

func (b *syslogBackend) stop() {
	for _, ch := range b.quitChans {
		close(ch)
	}
}

func (f *syslogForwarder) connect() (net.Conn, error) {
	conn, err := net.DialTimeout(f.url.Scheme, f.url.Host, forwardConnDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("[log forwarder] unable to connect to %q: %s", f.url, err)
	}
	if f.url.Scheme == "tcp" {
		conn = newBufferedConn(conn, time.Second)
	} else {
		f.messageLimit = f.mtu - udpHeaderSz
	}
	return conn, nil
}

func (f *syslogForwarder) splitParts(conn net.Conn, bufIdx bufferWithIdx) error {
	fullLen := len(bufIdx.buffer)
	if f.messageLimit <= 0 || fullLen <= f.messageLimit {
		// Fast path, message fit, no manipulation needed.
		return f.writePart(conn, bufIdx.buffer)
	}
	headerBuf := bufIdx.buffer[:bufIdx.headerIdx]
	trailerBuf := bufIdx.buffer[bufIdx.contentIdx:]
	contentBuf := bufIdx.buffer[bufIdx.headerIdx:bufIdx.contentIdx]
	availableSz := f.messageLimit - (len(headerBuf) + len(trailerBuf))
	contentSz := len(contentBuf)
	for contentSz > availableSz {
		buffer := f.bufferPool.Get().([]byte)[:0]
		buffer = append(buffer, headerBuf...)
		buffer = append(buffer, contentBuf[:availableSz]...)
		buffer = append(buffer, trailerBuf...)
		err := f.writePart(conn, buffer)
		f.bufferPool.Put(buffer)
		if err != nil {
			return err
		}
		contentBuf = contentBuf[availableSz:]
		contentSz = len(contentBuf)
	}
	if contentSz > 0 {
		// Reuse original buffer for remainder, no need for getting another
		// from the bufferPool.
		buffer := headerBuf
		buffer = append(buffer, contentBuf...)
		buffer = append(buffer, trailerBuf...)
		return f.writePart(conn, buffer)
	}
	return nil
}

func (f *syslogForwarder) process(conn net.Conn, msg LogMessage) error {
	bufIdx := msg.(bufferWithIdx)
	err := f.splitParts(conn, bufIdx)
	f.bufferPool.Put(bufIdx.buffer)
	return err
}

func (f *syslogForwarder) writePart(conn net.Conn, buf []byte) error {
	err := conn.SetWriteDeadline(time.Now().Add(forwardConnWriteTimeout))
	if err != nil {
		return err
	}
	lenMsg := len(buf)
	n, err := conn.Write(buf)
	if err != nil {
		return err
	}
	if n < lenMsg {
		return fmt.Errorf("[log forwarder] short write trying to write log to %q", conn.RemoteAddr())
	}
	return nil
}

func (f *syslogForwarder) close(conn net.Conn) {
	// Reset deadline, if we don't do this the connection remains open
	// on the other end (causing tests to fail) for some weird reason.
	conn.SetWriteDeadline(time.Time{})
	conn.Close()
}
