// mgo - MongoDB driver for Go
//
// Copyright (c) 2010-2012 - Gustavo Niemeyer <gustavo@niemeyer.net>
//
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice, this
//    list of conditions and the following disclaimer.
// 2. Redistributions in binary form must reproduce the above copyright notice,
//    this list of conditions and the following disclaimer in the documentation
//    and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
// ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE LIABLE FOR
// ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
// LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
// ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package mgo

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/CardInfoLink/mgo/bson"
	"github.com/CardInfoLink/mgo/internal/scram"
)

type authCmd struct {
	Authenticate int

	Nonce string
	User  string
	Key   string
}

type startSaslCmd struct {
	StartSASL int `bson:"startSasl"`
}

type authResult struct {
	ErrMsg string
	Ok     bool
}

type logoutCmd struct {
	Logout int
}

type saslCmd struct {
	Start          int    `bson:"saslStart,omitempty"`
	Continue       int    `bson:"saslContinue,omitempty"`
	ConversationId int    `bson:"conversationId,omitempty"`
	Mechanism      string `bson:"mechanism,omitempty"`
	Payload        []byte
}

type authX509Cmd struct {
	Authenticate int
	User         string
	Mechanism    string
}

type saslResult struct {
	Ok    bool `bson:"ok"`
	NotOk bool `bson:"code"` // Server <= 2.3.2 returns ok=1 & code>0 on errors (WTF?)
	Done  bool

	ConversationId int `bson:"conversationId"`
	Payload        []byte
	ErrMsg         string
}

type saslStepper interface {
	Step(serverData []byte) (clientData []byte, done bool, err error)
	Close()
}

func (socket *mongoSocket) Login(cred Credential) error {
	socket.Lock()
	// 默认使用 SCRAM-SHA-1 认证
	if cred.Mechanism == "" {
		cred.Mechanism = "SCRAM-SHA-1"
	}
	for _, sockCred := range socket.creds {
		if sockCred == cred {
			debugf("Socket %p to %s: login: db=%q user=%q (already logged in)", socket, socket.addr, cred.Source, cred.Username)
			socket.Unlock()
			return nil
		}
	}
	if socket.dropLogout(cred) {
		debugf("Socket %p to %s: login: db=%q user=%q (cached)", socket, socket.addr, cred.Source, cred.Username)
		socket.creds = append(socket.creds, cred)
		socket.Unlock()
		return nil
	}
	socket.Unlock()

	debugf("Socket %p to %s: login: db=%q user=%q", socket, socket.addr, cred.Source, cred.Username)

	var err error
	switch cred.Mechanism {
	case "MONGODB-CR", "MONGO-CR":
		// 不再支持旧的认证机制
		err = fmt.Errorf("authentication mechanism %q is no longer supported, please use SCRAM-SHA-1 or SCRAM-SHA-256", cred.Mechanism)
	case "PLAIN":
		err = socket.loginPlain(cred)
	case "MONGODB-X509":
		err = socket.loginX509(cred)
	default:
		// 使用 SASL 进行认证
		err = socket.loginSASL(cred)
	}

	if err != nil {
		debugf("Socket %p to %s: login error: %s", socket, socket.addr, err)
	} else {
		debugf("Socket %p to %s: login successful", socket, socket.addr)
	}
	return err
}

func (socket *mongoSocket) loginClassic(cred Credential) error {
	// 不再支持旧的认证机制
	return fmt.Errorf("MONGODB-CR authentication is no longer supported, please use SCRAM-SHA-1 or SCRAM-SHA-256")
}

func (socket *mongoSocket) loginX509(cred Credential) error {
	cmd := authX509Cmd{Authenticate: 1, User: cred.Username, Mechanism: "MONGODB-X509"}
	res := authResult{}
	return socket.loginRun(cred.Source, &cmd, &res, func() error {
		if !res.Ok {
			return errors.New(res.ErrMsg)
		}
		socket.Lock()
		socket.dropAuth(cred.Source)
		socket.creds = append(socket.creds, cred)
		socket.Unlock()
		return nil
	})
}

func (socket *mongoSocket) loginPlain(cred Credential) error {
	cmd := saslCmd{Start: 1, Mechanism: "PLAIN", Payload: []byte("\x00" + cred.Username + "\x00" + cred.Password)}
	res := authResult{}
	return socket.loginRun(cred.Source, &cmd, &res, func() error {
		if !res.Ok {
			return errors.New(res.ErrMsg)
		}
		socket.Lock()
		socket.dropAuth(cred.Source)
		socket.creds = append(socket.creds, cred)
		socket.Unlock()
		return nil
	})
}

func (socket *mongoSocket) loginSASL(cred Credential) error {
	var sasl saslStepper
	var err error
	if cred.Mechanism == "SCRAM-SHA-1" {
		// SCRAM is handled without external libraries.
		sasl = saslNewScram(cred)
	} else if len(cred.ServiceHost) > 0 {
		sasl, err = saslNew(cred, cred.ServiceHost)
	} else {
		sasl, err = saslNew(cred, socket.Server().Addr)
	}
	if err != nil {
		return err
	}
	defer sasl.Close()

	// The goal of this logic is to carry a locked socket until the
	// local SASL step confirms the auth is valid; the socket needs to be
	// locked so that concurrent action doesn't leave the socket in an
	// auth state that doesn't reflect the operations that took place.
	// As a simple case, imagine inverting login=>logout to logout=>login.
	//
	// The logic below works because the lock func isn't called concurrently.
	locked := false
	lock := func(b bool) {
		if locked != b {
			locked = b
			if b {
				socket.Lock()
			} else {
				socket.Unlock()
			}
		}
	}

	lock(true)
	defer lock(false)

	start := 1
	cmd := saslCmd{}
	res := saslResult{}
	for {
		payload, done, err := sasl.Step(res.Payload)
		if err != nil {
			return err
		}
		if done && res.Done {
			socket.dropAuth(cred.Source)
			socket.creds = append(socket.creds, cred)
			break
		}
		lock(false)

		cmd = saslCmd{
			Start:          start,
			Continue:       1 - start,
			ConversationId: res.ConversationId,
			Mechanism:      cred.Mechanism,
			Payload:        payload,
		}
		start = 0
		cred.Mechanism = ""
		err = socket.loginRun(cred.Source, &cmd, &res, func() error {
			// See the comment on lock for why this is necessary.
			lock(true)
			if !res.Ok || res.NotOk {
				return fmt.Errorf("server returned error on SASL authentication step: %s", res.ErrMsg)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if done && res.Done {
			socket.dropAuth(cred.Source)
			socket.creds = append(socket.creds, cred)
			break
		}
	}

	return nil
}

func saslNewScram(cred Credential) *saslScram {
	credsum := md5.New()
	credsum.Write([]byte(cred.Username + ":mongo:" + cred.Password))
	client := scram.NewClient(sha1.New, cred.Username, hex.EncodeToString(credsum.Sum(nil)))
	return &saslScram{cred: cred, client: client}
}

type saslScram struct {
	cred   Credential
	client *scram.Client
}

func (s *saslScram) Close() {}

func (s *saslScram) Step(serverData []byte) (clientData []byte, done bool, err error) {
	more := s.client.Step(serverData)
	return s.client.Out(), !more, s.client.Err()
}

func (socket *mongoSocket) loginRun(db string, query, result interface{}, f func() error) error {
	var mutex sync.Mutex
	var replyErr error
	mutex.Lock()

	op := queryOp{}
	op.query = query
	op.collection = db + ".$cmd"
	op.limit = -1
	op.replyFunc = func(err error, reply *replyOp, docNum int, docData []byte) {
		defer mutex.Unlock()

		if err != nil {
			replyErr = err
			return
		}

		err = bson.Unmarshal(docData, result)
		if err != nil {
			replyErr = err
		} else {
			// Must handle this within the read loop for the socket, so
			// that concurrent login requests are properly ordered.
			replyErr = f()
		}
	}

	err := socket.Query(&op)
	if err != nil {
		return err
	}
	mutex.Lock() // Wait.
	return replyErr
}

func (socket *mongoSocket) Logout(db string) {
	socket.Lock()
	cred, found := socket.dropAuth(db)
	if found {
		debugf("Socket %p to %s: logout: db=%q (flagged)", socket, socket.addr, db)
		socket.logout = append(socket.logout, cred)
	}
	socket.Unlock()
}

func (socket *mongoSocket) LogoutAll() {
	socket.Lock()
	if l := len(socket.creds); l > 0 {
		debugf("Socket %p to %s: logout all (flagged %d)", socket, socket.addr, l)
		socket.logout = append(socket.logout, socket.creds...)
		socket.creds = socket.creds[0:0]
	}
	socket.Unlock()
}

func (socket *mongoSocket) flushLogout() (ops []interface{}) {
	socket.Lock()
	if l := len(socket.logout); l > 0 {
		debugf("Socket %p to %s: logout all (flushing %d)", socket, socket.addr, l)
		for i := 0; i != l; i++ {
			op := queryOp{}
			op.query = &logoutCmd{1}
			op.collection = socket.logout[i].Source + ".$cmd"
			op.limit = -1
			ops = append(ops, &op)
		}
		socket.logout = socket.logout[0:0]
	}
	socket.Unlock()
	return
}

func (socket *mongoSocket) dropAuth(db string) (cred Credential, found bool) {
	for i, sockCred := range socket.creds {
		if sockCred.Source == db {
			copy(socket.creds[i:], socket.creds[i+1:])
			socket.creds = socket.creds[:len(socket.creds)-1]
			return sockCred, true
		}
	}
	return cred, false
}

func (socket *mongoSocket) dropLogout(cred Credential) (found bool) {
	for i, sockCred := range socket.logout {
		if sockCred == cred {
			copy(socket.logout[i:], socket.logout[i+1:])
			socket.logout = socket.logout[:len(socket.logout)-1]
			return true
		}
	}
	return false
}
