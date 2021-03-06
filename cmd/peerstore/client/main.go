package main

import (
	"bytes"
	"crypto/aes"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/dietsche/rfsnotify"
	"github.com/golang/glog"
	"github.com/husobee/peerstore/crypto"
	"github.com/husobee/peerstore/models"
	"github.com/husobee/peerstore/protocol"
	"github.com/pkg/errors"
	"gopkg.in/fsnotify.v1"
)

var (
	peerAddr string
	// peerKeyFile - the key file location for a known peer on the network
	peerKeyFile      string
	selfKeyFile      string
	shareWithKeyFile string
	localPath        string
	operation        string
	filename         string
	filedest         string
	pollInterval     time.Duration
)

func init() {
	flag.StringVar(
		&peerAddr, "peerAddr", "",
		"the address of a peer")
	flag.StringVar(
		&operation, "operation", "",
		"choice of operation, backup or getfile.  backup will put localPath in peerstore, getfile will download the file and put it in filedest. specify the file to download by name with -filename flag")
	flag.StringVar(
		&localPath, "localPath", "",
		"the location of the dir you wish to sync")
	flag.StringVar(
		&filename, "filename", "",
		"the name of the file you want to get from peerstore")
	flag.StringVar(
		&filedest, "filedest", "",
		"destination of the file with doing getfile operation")
	flag.StringVar(
		&peerKeyFile, "peerKeyFile", "",
		"the key file location of a known peer on the network")
	flag.StringVar(
		&selfKeyFile, "selfKeyFile", "",
		"the key file location of your private/public key pem file")
	flag.StringVar(
		&shareWithKeyFile, "shareWithKeyFile", "",
		"the key file location of the public key of the user you wish to share with as a pem file")
	flag.DurationVar(&pollInterval, "poll", time.Second, "the polling interval for sync")
	flag.Parse()
}

func validateParams() error {
	if peerAddr == "" {
		return errors.New("peerAddr must be set")
	}
	if operation == "backup" {
		if localPath == "" {
			return errors.New("localPath must be set")
		}
		info, err := os.Stat(localPath)
		if err != nil {
			return errors.Wrap(err, "error attempting to validate localPath: ")
		}
		if !info.IsDir() {
			return errors.New("localPath must be a valid directory")
		}
	} else if operation == "sync" {
		if localPath == "" {
			return errors.New("localPath must be set")
		}
		info, err := os.Stat(localPath)
		if err != nil {
			return errors.Wrap(err, "error attempting to validate localPath: ")
		}
		if !info.IsDir() {
			return errors.New("localPath must be a valid directory")
		}
	} else if operation == "getfile" {
		if filedest == "" {
			return errors.New("filedest must be set")
		}
		if filename == "" {
			return errors.New("filename must be set")
		}
	} else if operation == "share" {
		if filename == "" {
			return errors.New("filename must be set")
		}

	} else {
		return errors.New("must specify operation flag, either backup or getfile")
	}
	return nil
}

func main() {

	log.Println("starting client")

	if err := validateParams(); err != nil {
		log.Fatalf("could not validate params: %v\n", err)
	}

	var (
		privateKey *rsa.PrivateKey
		err        error
	)

	if _, err := os.Stat(selfKeyFile); err != nil {
		// generate our public key
		privateKey, err = crypto.GenerateKeyPair()
		if err != nil {
			log.Printf("failed to generate keypair: %s", err)
			return
		}
		// create our keypair file:
		keyFile, err := os.Create(fmt.Sprintf("%s", selfKeyFile))
		if err != nil {
			glog.Infof("failed to create keypair file: %s", err)
			return
		}
		crypto.WritePrivateKeyAsPem(keyFile, privateKey)
		crypto.WritePublicKeyAsPem(keyFile, privateKey.Public().(*rsa.PublicKey))
		keyFile.Close()
	} else {
		keyFile, err := os.Open(fmt.Sprintf("%s", selfKeyFile))
		privateKey, err = crypto.ReadKeypairAsPem(keyFile)
		if err != nil {
			log.Printf("failed to read keypair: %s", err)
			return
		}
	}

	kb, _ := crypto.GobEncodePublicKey(privateKey.Public().(*rsa.PublicKey))
	id := models.Identifier(sha1.Sum(kb))

	// read in our peer's public key
	keyFile, err := os.Open(peerKeyFile) // For read access.
	if err != nil {
		glog.Infof("failed to read initial peer key file: %s", err)
		return
	}

	peerKey, err := crypto.ReadPublicKeyAsPem(keyFile)
	if err != nil {
		glog.Infof("failed to read keypair file: %s", err)
		return
	}

	// register the user with the network
	log.Printf("usertype should be : %d", protocol.UserType)
	rt, err := protocol.NewTransport("tcp", peerAddr, protocol.UserType, id, &peerKey, privateKey)
	if err != nil {
		log.Printf("ERR: %v", err)
		return
	}
	log.Println("transport established")

	resp, err := rt.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			From:   id,
			Type:   protocol.UserType,
			PubKey: privateKey.Public().(*rsa.PublicKey),
		},
		Method: protocol.UserRegistrationMethod,
	})
	log.Println("registered user")
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
		return
	}
	rt.Close()
	log.Printf("response: %+v", resp)

	var peer = models.Node{
		Addr:      peerAddr,
		PublicKey: &peerKey,
	}

	switch operation {
	case "share":
		log.Println("starting share!")

		var shareWithKey rsa.PublicKey

		// check that we have a valid shareWithKeyFile
		_, err := os.Stat(shareWithKeyFile)
		if !handleError(err) {
			return
		}

		keyFile, err := os.Open(fmt.Sprintf("%s", shareWithKeyFile))
		shareWithKey, err = crypto.ReadPublicKeyAsPem(keyFile)
		if !handleError(err) {
			return
		}
		// generate the ID of the user we are sharing with
		gobKey, err := crypto.GobEncodePublicKey(&shareWithKey)
		if !handleError(err) {
			return
		}
		shareWithID := models.Identifier(sha1.Sum(gobKey))

		// we have our shareWithKey, which we will use to encrypt
		// the session key

		// create a transport to our peer
		t, err := createTransport(id, peer, privateKey)
		if !handleError(err) {
			return
		}
		defer t.Close()
		// get the node that has the file
		node, err := getNode(fileToKeyIdentifier(filename), id, t)
		// connect to node housing the data
		st, err := createTransport(id, node, privateKey)
		if !handleError(err) {
			return
		}
		defer st.Close()
		// get the file
		resp, err := getKey(fileToKeyIdentifier(filename), id, st)
		if !handleError(err) {
			return
		}
		// encrypt session key with public key of shared user
		sKey, err := crypto.DecryptRSA(privateKey, resp.Header.Secret)
		if !handleError(err) {
			return
		}

		// use the shareWithKeyFile to add the share with user's
		// id and encrypted session key from their public key
		encSessionKey, err := crypto.EncryptRSA(&shareWithKey, sKey)
		if !handleError(err) {
			return
		}

		// populate SharedWith header, shared user's id/encrypted key
		sharedWith := []protocol.SharedSecret{
			protocol.SharedSecret{
				ID:     shareWithID,
				Secret: encSessionKey,
			},
		}

		// post file
		log.Println("starting request: ", protocol.PostFileMethod)
		_, err = st.RoundTrip(&protocol.Request{
			Header: protocol.Header{
				Key:          fileToKeyIdentifier(filename),
				Type:         protocol.UserType,
				From:         id,
				DataLength:   uint64(len(resp.Data)),
				PubKey:       privateKey.Public().(*rsa.PublicKey),
				ResourceName: filename,
				Log:          true,
				SharedWith:   sharedWith,
				Secret:       resp.Header.Secret,
			},
			Method: protocol.PostFileMethod,
			Data:   resp.Data,
		})
		if !handleError(err) {
			return
		}

	case "sync":
		log.Println("starting sync!")

		var (
			quitChan   = make(chan bool)
			signalChan = make(chan os.Signal)
		)
		// need to kickoff a lookup to the transaction log in the DHT
		// if there is a transaction log, we need to perform a get on all the
		// resources that are listed in the transaction log and update our
		// transaction log

		// need to kick off an fsnotify to watch for changes to files
		// (except when we make changes from the sync)
		watcher, err := rfsnotify.NewWatcher()
		if err != nil {
			log.Printf("failed to start fs watcher: %s", err)
			os.Exit(1)
		}
		defer watcher.Close()
		log.Println("sync watcher has been created")

		// watch for an interrupt
		signal.Notify(signalChan, os.Interrupt)
		go func() {
			for _ = range signalChan {
				log.Print("Interrupt, Killing workers")
				// signal server to quit processing requests
				quitChan <- true
			}
		}()

		// initialize based on localPath and remote transaction log
		// we will pull the transaction log for this user.
		// given the remote transaction log walk the localPath...
		// if the localPath contains files that are not in the transaction
		// log, then perform uploads of those files just like the backup flag,
		// for each resource in the transaction log, check the timestamp,
		// if the timestamp is greater than current clock then pull
		// that resource.  If timestamp is less than current clock, then post
		var transactionLog = models.TransactionLog{}
		transactionLog, _ = Synchronize(
			id, localPath, models.Node{Addr: peerAddr, PublicKey: &peerKey},
			privateKey, transactionLog)

		AddWatchers(watcher, localPath)

		log.Println("starting signal loop")
		for {
			select {
			case <-quitChan:
				os.Exit(0)
			case <-time.After(pollInterval):
				// get the transaction log, look for differences
				// if differences, get the resources that are different
				RemoveWatchers(watcher, localPath)
				transactionLog, _ = Synchronize(
					id, localPath, models.Node{Addr: peerAddr, PublicKey: &peerKey},
					privateKey, transactionLog)
				AddWatchers(watcher, localPath)
			case event := <-watcher.Events:
				// we got a filesystem event, pull remote transaction log
				// update it accordingly and save
				if event.Op == fsnotify.Write {
					log.Println("file written: ", event.Name)
					path := strings.TrimPrefix(event.Name, localPath)
					PostFile(id, path, models.Node{Addr: peerAddr, PublicKey: &peerKey},
						privateKey)
				}
				if event.Op == fsnotify.Remove {
					log.Println("file removed: ", event.Name)
					path := strings.TrimPrefix(event.Name, localPath)
					DeleteFile(id, path, models.Node{Addr: peerAddr, PublicKey: &peerKey},
						privateKey)
				}
			case err := <-watcher.Errors:
				// somthing terrible happened with our FS watcher
				log.Printf("fs watcher error: %s", err)
				os.Exit(1)
			}
		}

	case "backup":
		var walkFn = func(path string, fi os.FileInfo, err error) error {
			if !fi.IsDir() {
				log.Printf("file is: %s\n", path)

				// figure out where to connect to
				t, err := createTransport(id, peer, privateKey)
				if !handleError(err) {
					return errors.Wrap(err, "failed to create transport")
				}
				defer t.Close()

				node, err := getNode(fileToKeyIdentifier(path), id, t)
				if !handleError(err) {
					return errors.Wrap(err, "failed to get node")
				}

				st, err := createTransport(id, node, privateKey)
				if !handleError(err) {
					return errors.Wrap(err, "failed to create transport")
				}
				defer st.Close()

				// see if file exists, in order to get secret
				var (
					sessionKey []byte
					secret     []byte
					iv         []byte
					ciphertext []byte
				)

				// read the file
				plaintext, err := ioutil.ReadFile(path)

				resp, err := getKey(fileToKeyIdentifier(path), id, t)
				fmt.Println("UHHHH! ", err, resp.Status)
				if err != nil || resp.Status == protocol.Error {
					// doesnt exist, create new key
					log.Println("IN HER$E!!!")
					sessionKey, secret, err = crypto.GenerateSessionKey(
						privateKey.Public().(*rsa.PublicKey))
					log.Printf("plaintext session key: %s", hex.EncodeToString(sessionKey))
					log.Printf("crypted session key: %s", hex.EncodeToString(secret))
					log.Printf("len of session key crypted: %d", len(secret))
					if !handleError(err) {
						return errors.Wrap(err, "failed to generate session key")
					}
					ciphertext, iv, err = crypto.Encrypt(sessionKey, plaintext)
					if !handleError(err) {
						return errors.Wrap(err, "failed to encrypt payload")
					}
				} else {
					// user session key from remote
					secret = resp.Header.Secret
					sessionKey, err = crypto.DecryptRSA(privateKey, secret)
					log.Printf("plaintext session key: %s", hex.EncodeToString(sessionKey))
					log.Printf("crypted session key: %s", hex.EncodeToString(secret))
					log.Printf("len of session key crypted: %d", len(secret))
					if !handleError(err) {
						return errors.Wrap(err, "failed to decrypt session Key")
					}
					iv = resp.Data[:aes.BlockSize]
					ciphertext, iv, err = crypto.EncryptWithIV(sessionKey, plaintext, iv)
					if !handleError(err) {
						return errors.Wrap(err, "failed to encrypt payload")
					}
				}

				log.Printf("plaintext is: %s", string(plaintext))

				log.Printf("len of ciphertext: %d", len(ciphertext))
				log.Printf("ciphertext: %s", hex.EncodeToString(ciphertext))
				log.Printf("len of iv: %d", len(iv))
				log.Printf("iv: %s", hex.EncodeToString(iv))
				ciphertext = append(iv, ciphertext...)

				// send the file over
				log.Println("starting request: ", protocol.PostFileMethod)
				_, err = st.RoundTrip(&protocol.Request{
					Header: protocol.Header{
						Key:          fileToKeyIdentifier(path),
						Type:         protocol.UserType,
						From:         id,
						DataLength:   uint64(len(ciphertext)),
						PubKey:       privateKey.Public().(*rsa.PublicKey),
						ResourceName: path,
						Log:          true,
						Secret:       secret,
					},
					Method: protocol.PostFileMethod,
					Data:   ciphertext,
				})
				if !handleError(err) {
					return errors.Wrap(err, "failed to post file")
				}
			}
			return nil
		}

		// Open up directory
		// read each file, and send to peerAddr
		filepath.Walk(localPath, walkFn)

	case "getfile":
		log.Printf("getting file: %s, putting %s", filename, filedest)
		t, err := createTransport(id, peer, privateKey)
		if !handleError(err) {
			return
		}
		defer t.Close()

		// get the node that houses the file we need
		node, err := getNode(fileToKeyIdentifier(filename), id, t)

		st, err := createTransport(id, node, privateKey)
		if !handleError(err) {
			return
		}
		defer st.Close()

		// get the key
		resp, err := getKey(fileToKeyIdentifier(filename), id, t)
		if !handleError(err) {
			return
		}

		log.Printf("response from getKey: %+v", resp)
		log.Printf("secret from getKey: %+v", hex.EncodeToString(resp.Header.Secret))
		// get the secret from the header,
		// decrypt secret
		sessionKey, err := crypto.DecryptRSA(privateKey, resp.Header.Secret)
		if !handleError(err) {
			return
		}

		log.Printf("plaintext session key is: %s", hex.EncodeToString(sessionKey))

		// pull iv out of data
		log.Printf("length of data: %d", len(resp.Data))
		iv := resp.Data[:aes.BlockSize]
		ciphertext := resp.Data[aes.BlockSize:]

		log.Printf("iv from data: %s", hex.EncodeToString(iv))
		log.Printf("ciphertext from data: %s", hex.EncodeToString(ciphertext))

		// decrypt data
		plaintext, err := crypto.Decrypt(sessionKey, ciphertext, iv)
		if !handleError(err) {
			log.Printf("err: %s", err.Error())
			return
		}
		// store data

		log.Printf("plaintext is: %s", plaintext)

		err = ioutil.WriteFile(filedest, plaintext, 0644)
		if err != nil {
			log.Println(err)
			return
		}
	}
}

func fileToKeyIdentifier(filename string) models.Identifier {
	return models.Identifier(sha1.Sum([]byte(filename)))
}

func getNode(key, id models.Identifier, t *protocol.Transport) (models.Node, error) {
	// serialize our get successor request
	var (
		idBuf = new(bytes.Buffer)
		node  = models.Node{}
		enc   = gob.NewEncoder(idBuf)
	)
	// encode successor request
	enc.Encode(models.SuccessorRequest{key})
	// perform round trip on transport
	resp, err := t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: id,
			Key:  key,
		},
		Method: protocol.GetSuccessorMethod,
		Data:   idBuf.Bytes(),
	})
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
		return node, errors.Wrap(err, "failed round trip to find successor")
	}

	log.Printf("found node")

	dec := gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&node)
	if err != nil {
		log.Printf("Failed to deserialize the node data: %v", err)
		return node, errors.Wrap(err, "failed to deserialize node data")
	}
	return node, nil
}

func createTransport(id models.Identifier, node models.Node, key *rsa.PrivateKey) (*protocol.Transport, error) {
	return protocol.NewTransport(
		"tcp", node.Addr, protocol.UserType, id, node.PublicKey, key)
}

func handleError(err error) bool {
	if err != nil {
		log.Printf("ERR: %v", err)
		return false
	}
	return true
}

func getKey(key, id models.Identifier, t *protocol.Transport) (protocol.Response, error) {
	// perform round trip
	resp, err := t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: id,
			Key:  key,
		},
		Method: protocol.GetFileMethod,
	})
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
		return protocol.Response{}, errors.Wrap(err, "failed round trip")
	}
	if resp.Status == protocol.Error {
		log.Printf("failed to get resource requested.")
		return resp, errors.New("protocol failure")
	}
	return resp, nil
}

var tl = models.TransactionLog{}

func Synchronize(clientID models.Identifier, localPath string, peer models.Node, privateKey *rsa.PrivateKey, oldTransactionLog models.TransactionLog) (models.TransactionLog, error) {
	// pull transaction log
	tl, err := GetTransactionLog(
		clientID, peer, privateKey.Public().(*rsa.PublicKey), privateKey)

	log.Printf("local transaction log: %+v", tl)
	log.Printf("remote transaction log: %+v", tl)

	if err != nil {
		log.Printf("Error getting transaction log: %s", err)
	}
	// walk directory, if file is not in transaction log post it
	var walkFn = func(path string, fi os.FileInfo, err error) error {
		// use relative path
		path = strings.TrimPrefix(path, localPath)

		if !fi.IsDir() {
			log.Printf("file is: %s\n", path)
			log.Printf("path is: %s", path)
			if _, ok := tl[path]; !ok {
				// remote has never seen this one, post it
				log.Printf("path does not exist in tl")
				PostFile(clientID, path, peer, privateKey)
			}
		}
		return nil
	}

	// walk directory
	filepath.Walk(localPath, walkFn)

	// now we need to go through the transaction log and pull any new
	// resources, will omit resources we have already seen
	for k, v := range tl {

		lastEntry := v.Entries[0]
		for i, _ := range v.Entries {
			if v.Entries[i].Timestamp >= lastEntry.Timestamp {
				lastEntry = v.Entries[i]
			}
		}

		log.Printf("Last Entry: %v", lastEntry)

		// check if this entry is in our local transaction log
		if _, ok := oldTransactionLog[k]; !ok {
			// not in our old transaction log, so we should get this thing
			GetFile(clientID, k, peer, privateKey)
			continue
		}
		oldLastEntry := oldTransactionLog[k].Entries[0]
		for i, _ := range oldTransactionLog[k].Entries {
			if oldTransactionLog[k].Entries[i].Timestamp >= oldLastEntry.Timestamp {
				oldLastEntry = oldTransactionLog[k].Entries[i]
			}
		}

		log.Printf("oldlastentry time: %d, lastentrytime: %d", oldLastEntry.Timestamp, lastEntry.Timestamp)
		if oldLastEntry.Timestamp < lastEntry.Timestamp {
			// if the old log last entry is less than the new log last entry
			// then we need to get the latest change
			if lastEntry.Operation == models.DeleteOperation {
				log.Printf("remote says to delete, removing")
				// remote says remove, so remove
				os.Remove(filepath.Join(localPath, k))
				continue
			}
			log.Printf("Fetch the updated resource!")
			GetFile(clientID, k, peer, privateKey)
		} else if oldLastEntry.Timestamp == lastEntry.Timestamp {
			// do nothing!
		} else {
			// we have something locally that is newer.
			if oldLastEntry.Operation == models.DeleteOperation {
				DeleteFile(clientID, k, peer, privateKey)
				continue
			}
			PostFile(clientID, k, peer, privateKey)
		}
	}
	return tl, nil
}

func GetFile(clientID models.Identifier, path string, peer models.Node, privateKey *rsa.PrivateKey) {
	// get the specified resource from the DHT, and store it in path
	log.Printf("getting file: %s, putting %s", path, path)
	// the key for the distributed lookup
	key := sha1.Sum([]byte(path))

	// figure out where to connect to
	st, err := protocol.NewTransport("tcp", peer.Addr, protocol.UserType, clientID, peer.PublicKey, privateKey)
	if err != nil {
		log.Printf("ERR: %v", err)
	}

	// serialize our get successor request
	var idBuf = new(bytes.Buffer)
	enc := gob.NewEncoder(idBuf)
	enc.Encode(models.SuccessorRequest{
		models.Identifier(key),
	})
	resp, err := st.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: clientID,
			Key:  key,
		},
		Method: protocol.GetSuccessorMethod,
		Data:   idBuf.Bytes(),
	})

	st.Close()
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
		return
	}

	log.Printf("found node")

	// connect to that host for this file
	// pull node out of response, and connect to that host
	var node = models.Node{}
	dec := gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&node)
	if err != nil {
		log.Printf("Failed to deserialize the node data: %v", err)
		return
	}

	// figure out where to connect to
	t, err := protocol.NewTransport("tcp", node.Addr, protocol.UserType, clientID, node.PublicKey, privateKey)
	if err != nil {
		log.Printf("ERR: %v", err)
	}

	resp, err = t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: clientID,
			Key:  key,
		},
		Method: protocol.GetFileMethod,
	})
	t.Close()
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
		return
	}
	if resp.Status == protocol.Error {
		log.Printf("failed to get resource requested.")
		return
	}

	models.IncrementClock(resp.Header.Clock)

	// make the directory structure needed:
	dir, _ := filepath.Split(filepath.Join(localPath, path))
	os.MkdirAll(dir, 0700)

	log.Printf("The file contents are: %s", string(resp.Data))

	err = ioutil.WriteFile(filepath.Join(localPath, path), resp.Data, 0644)
	if err != nil {
		log.Println(err)
		return
	}
}

func PostFile(clientID models.Identifier, path string, peer models.Node, privateKey *rsa.PrivateKey) {
	// post the specified resource in the DHT
	// the key for the distributed lookup
	key := sha1.Sum([]byte(path))
	data, err := ioutil.ReadFile(filepath.Join(localPath, path)) // path is the path to the file.

	// figure out where to connect to
	st, err := protocol.NewTransport("tcp", peer.Addr, protocol.UserType, clientID, peer.PublicKey, privateKey)
	if err != nil {
		log.Printf("ERR: %v", err)
	}

	// serialize our get successor request
	var idBuf = new(bytes.Buffer)
	enc := gob.NewEncoder(idBuf)
	enc.Encode(models.SuccessorRequest{
		models.Identifier(key),
	})
	resp, err := st.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			From:   clientID,
			Type:   protocol.UserType,
			PubKey: privateKey.Public().(*rsa.PublicKey),
		},
		Method: protocol.GetSuccessorMethod,
		Data:   idBuf.Bytes(),
	})
	if err != nil {
		log.Printf("Failed to round trip the successor request: %v", err)
	}
	st.Close()

	// connect to that host for this file
	// pull node out of response, and connect to that host
	var node = models.Node{}
	dec := gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&node)
	if err != nil {
		log.Printf("Failed to deserialize the node data: %v", err)
	}

	// figure out where to connect to
	t, err := protocol.NewTransport("tcp", node.Addr, protocol.UserType, clientID, node.PublicKey, privateKey)
	if err != nil {
		log.Printf("ERR: %v", err)
	}

	// send the file over
	log.Println("starting request: ", protocol.PostFileMethod)
	response, err := t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Key:          key,
			Type:         protocol.UserType,
			From:         clientID,
			DataLength:   uint64(len(data)),
			PubKey:       privateKey.Public().(*rsa.PublicKey),
			ResourceName: path,
			Log:          true,
			Clock:        models.GetClock(),
		},
		Method: protocol.PostFileMethod,
		Data:   data,
	})
	t.Close()
	if err != nil {
		log.Printf("ERR: %v\n", err)
	}
	log.Printf("Response: %+v\n", response)
	// increment the clock
	models.IncrementClock(response.Header.Clock)

	tl, err := GetTransactionLog(clientID, node, privateKey.Public().(*rsa.PublicKey), privateKey)
	if err != nil {
		glog.Error("error getting transaction log: ", err)
	}

	var timestamp = models.GetClock()

	if entity, ok := tl[path]; ok {
		// entity exists, add entry
		entity.Entries = append(
			tl[path].Entries,
			models.TransactionEntry{
				Operation: models.UpdateOperation,
				ClientID:  clientID,
				Timestamp: timestamp,
			},
		)
		tl[path] = entity
	} else {
		// resource is not in transaction log
		tl[path] = models.TransactionEntity{
			ResourceName: path,
			ResourceID:   key,
			Entries: []models.TransactionEntry{
				models.TransactionEntry{
					Operation: models.UpdateOperation,
					ClientID:  clientID,
					Timestamp: timestamp,
				},
			},
		}
	}

	// Upload the serialized transaction log to the DHT
	err = PutTransactionLog(clientID, node, privateKey.Public().(*rsa.PublicKey), privateKey, tl)
	if err != nil {
		glog.Error("error putting transaction log: ", err)
	}

	t.Close()
}

func DeleteFile(clientID models.Identifier, path string, peer models.Node, privateKey *rsa.PrivateKey) {
	// delete the specified resource from the local file system
	key := sha1.Sum([]byte(path))

	tl, err := GetTransactionLog(clientID, peer, privateKey.Public().(*rsa.PublicKey), privateKey)
	if err != nil {
		glog.Error("error getting transaction log: ", err)
	}

	var timestamp = models.GetClock()

	if entity, ok := tl[path]; ok {
		// entity exists, add entry
		entity.Entries = append(
			tl[path].Entries,
			models.TransactionEntry{
				Operation: models.DeleteOperation,
				ClientID:  clientID,
				Timestamp: timestamp,
			},
		)
		tl[path] = entity
	} else {
		// resource is not in transaction log
		tl[path] = models.TransactionEntity{
			ResourceName: path,
			ResourceID:   key,
			Entries: []models.TransactionEntry{
				models.TransactionEntry{
					Operation: models.DeleteOperation,
					ClientID:  clientID,
					Timestamp: timestamp,
				},
			},
		}
	}

	// Upload the serialized transaction log to the DHT
	err = PutTransactionLog(clientID, peer, privateKey.Public().(*rsa.PublicKey), privateKey, tl)
	if err != nil {
		glog.Error("error putting transaction log: ", err)
	}
}

func GetTransactionLog(thisID models.Identifier, peer models.Node, userKey *rsa.PublicKey, selfKey *rsa.PrivateKey) (models.TransactionLog, error) {
	gobKey, _ := crypto.GobEncodePublicKey(userKey)
	id := models.Identifier(sha1.Sum(append(gobKey, []byte("-transaction-log")...)))

	log.Printf("Trying to GET Transaction LOG, ID: %x", id)

	// create a connection to our peer
	t, err := protocol.NewTransport("tcp", peer.Addr, protocol.UserType, id, peer.PublicKey, selfKey)
	if err != nil {
		glog.Error("ERR: %v", err)
	}

	var buf = new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	// Perform a Successor Request to our peer
	enc.Encode(models.SuccessorRequest{
		models.Identifier(id),
	})
	resp, err := t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: thisID,
			Key:  id,
		},
		Method: protocol.GetSuccessorMethod,
		Data:   buf.Bytes(),
	})
	t.Close()
	if err != nil {
		glog.Info("Failed to round trip the successor request: %v", err)
		return models.TransactionLog{}, errors.Wrap(err, "failed to get successor: ")
	}

	// populate our peer to get the log
	var node = models.Node{}
	dec := gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&node)
	if err != nil {
		glog.Error("Failed to deserialize the node data: %v", err)
		return models.TransactionLog{}, errors.Wrap(err, "failed deserialize successor: ")
	}

	glog.Info("Peer holding TransactionLog: %s", node.ToString())

	// now connect to the node holding the transaction log
	st, err := protocol.NewTransport("tcp", peer.Addr, protocol.UserType, thisID, node.PublicKey, selfKey)
	if err != nil {
		log.Printf("ERR: %v", err)
	}
	resp, err = st.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type:   protocol.UserType,
			From:   thisID,
			Key:    id,
			PubKey: selfKey.Public().(*rsa.PublicKey),
		},
		Method: protocol.GetFileMethod,
	})
	st.Close()
	if err != nil {
		log.Printf("Failed to round trip the get file request: %v", err)
		return models.TransactionLog{}, errors.Wrap(err, "failed to get file")
	}

	if resp.Status == protocol.Error {
		log.Printf("failed to get resource requested.")
		return models.TransactionLog{}, errors.Wrap(err, "failed to get file, protocol error")
	}

	var transactionLog = models.TransactionLog{}
	dec = gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&transactionLog)
	if err != nil {
		glog.Error("Failed to deserialize the transactionLog data: %v", err)
		return models.TransactionLog{}, errors.Wrap(err, "failed deserialize transaction log: ")
	}

	return transactionLog, nil
}

func PutTransactionLog(thisID models.Identifier, peer models.Node, userKey *rsa.PublicKey, selfKey *rsa.PrivateKey, transactionLog models.TransactionLog) error {
	gobKey, _ := crypto.GobEncodePublicKey(userKey)
	glog.Infof("userKey bytes: %x", userKey)
	glog.Infof("gobKey bytes: %x", gobKey)
	id := models.Identifier(sha1.Sum(append(gobKey, []byte("-transaction-log")...)))

	glog.Infof("Trying to PUT Transaction LOG, ID: %x", id)

	// create a connection to our peer
	t, err := protocol.NewTransport("tcp", peer.Addr, protocol.UserType, id, peer.PublicKey, selfKey)
	if err != nil {
		glog.Error("ERR: %v", err)
	}

	var buf = new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	// Perform a Successor Request to our peer
	enc.Encode(models.SuccessorRequest{
		models.Identifier(id),
	})
	resp, err := t.RoundTrip(&protocol.Request{
		Header: protocol.Header{
			Type: protocol.UserType,
			From: thisID,
			Key:  id,
		},
		Method: protocol.GetSuccessorMethod,
		Data:   buf.Bytes(),
	})
	t.Close()
	if err != nil {
		glog.Info("Failed to round trip the successor request: %v", err)
		return errors.Wrap(err, "failed to get successor: ")
	}
	// populate our peer to get the log
	var node = models.Node{}
	dec := gob.NewDecoder(bytes.NewBuffer(resp.Data))
	err = dec.Decode(&node)
	if err != nil {
		glog.Error("Failed to deserialize the node data: %v", err)
		return errors.Wrap(err, "failed deserialize successor: ")
	}

	glog.Info("Peer holding TransactionLog: %s", node.ToString())

	// encode the transaction log, and put to our node
	var logBuf = bytes.NewBuffer([]byte{})
	enc = gob.NewEncoder(logBuf)
	err = enc.Encode(&transactionLog)
	if err != nil {
		glog.Error("Failed to serialize the transactionLog data: %v", err)
		return errors.Wrap(err, "failed serialize transaction log: ")
	}

	// figure out where to connect to
	st, err := protocol.NewTransport("tcp", node.Addr, protocol.UserType, id, node.PublicKey, selfKey)
	if err != nil {
		glog.Error("ERR: %v", err)
		return errors.Wrap(err, "failed serialize transaction log: ")
	}

	// send the file over
	glog.Info("starting request: ", protocol.PostFileMethod)
	request := &protocol.Request{
		Header: protocol.Header{
			Key:        id,
			Type:       protocol.UserType,
			From:       thisID,
			DataLength: uint64(len(logBuf.Bytes())),
			PubKey:     selfKey.Public().(*rsa.PublicKey),
		},
		Method: protocol.PostFileMethod,
		Data:   logBuf.Bytes(),
	}

	response, err := st.RoundTrip(request)
	models.IncrementClock(response.Header.Clock)
	st.Close()
	if err != nil {
		glog.Error("ERR: %v\n", err)
		return errors.Wrap(err, "failed serialize transaction log: ")
	}
	log.Printf("!!!!!!!!!!!!!!!!! PUT TRANSACTION LOG !!!!!!!!!!!! Response: %+v\n", response)

	return nil

}

func AddWatchers(watcher *rfsnotify.RWatcher, basePath string) {
	// walk all subdirectories
	// set the watcher to watch the localpath
	watcher.AddRecursive(basePath)
}

func RemoveWatchers(watcher *rfsnotify.RWatcher, basePath string) {
	// walk all subdirectories
	// set the watcher to watch the localpath
	watcher.RemoveRecursive(basePath)
}
