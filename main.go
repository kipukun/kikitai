package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/dhowden/tag"
	"github.com/gorilla/mux"
	bolt "go.etcd.io/bbolt"
)

type state struct {
	db        *bolt.DB
	srv       *http.Server
	templates *template.Template
	path      string
}

type song struct {
	Path                              string
	Title, Artist, Album, AlbumArtist string
}

func c(path string, w io.Writer, ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-i", path, "-c:a", "libopus", "-b:a", "128k", "-map", "0:0", "-f", "opus", "-")
	cmd.Stdout = w
	//	cmd.Stderr = os.Stdout
	return cmd
}

func (s *state) home(w http.ResponseWriter, r *http.Request) {
	songs := make(map[string]song)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("songs"))
		err := b.ForEach(func(k, v []byte) error {
			var s song
			dec := gob.NewDecoder(bytes.NewReader(v))
			err := dec.Decode(&s)
			if err != nil {
				return err
			}
			songs[string(k)] = s
			return nil
		})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
		return
	}
	s.templates.ExecuteTemplate(w, "main.tmpl.html", songs)
}

func (s *state) audio(w http.ResponseWriter, r *http.Request) {
	var so song
	vars := mux.Vars(r)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("songs"))
		bs := b.Get([]byte(vars["hash"]))
		dec := gob.NewDecoder(bytes.NewReader(bs))
		err := dec.Decode(&so)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
		return
	}
	w.Header().Set("Content-Type", "audio/opus")
	w.Header().Set("Content-Disposition", `attachment; filename="audio.opus"`)

	pr, pw := io.Pipe()
	cmd := c(so.Path, pw, r.Context())
	go func() {
		err := cmd.Run()
		defer pw.Close()
		if err != nil {
			fmt.Println(err)
			return
		}
	}()
	io.Copy(w, pr)
}

func (s *state) update() error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("songs"))
		buf := new(bytes.Buffer)
		enc := gob.NewEncoder(buf)

		err := filepath.Walk(s.path, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				fmt.Printf("prevent panic by handling failure accessing a path %q: %v\n", path, err)
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, walkErr := os.Open(path)
			if walkErr != nil {
				return walkErr
			}

			m, walkErr := tag.ReadFrom(f)
			if err != nil {
				return walkErr
			}

			h := sha1.New()
			h.Write([]byte(path))
			key := make([]byte, hex.EncodedLen(len(h.Sum(nil))))
			hex.Encode(key, h.Sum(nil))

			value := song{
				Path:        path,
				Title:       m.Title(),
				Album:       m.Album(),
				AlbumArtist: m.AlbumArtist(),
				Artist:      m.Artist(),
			}

			walkErr = enc.Encode(value)
			if walkErr != nil {
				return walkErr
			}

			walkErr = b.Put(key, buf.Bytes())
			defer buf.Reset()
			if walkErr != nil {
				return walkErr
			}

			return nil
		})
		return err
	})
	return err
}

func main() {
	var err error
	s := new(state)
	mux := mux.NewRouter()
	s.srv = &http.Server{
		Handler: mux,
		Addr:    ":8080",
	}
	mux.HandleFunc("/{hash}/transcode", s.audio)
	mux.HandleFunc("/", s.home)

	s.templates = template.Must(template.ParseGlob("*.tmpl.html"))
	s.path = `.\audio`

	s.db, err = bolt.Open("music.db", 0667, nil)
	if err != nil {
		log.Fatalln(err)
	}
	s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("songs"))
		return err
	})

	ch := make(chan os.Signal)
	signal.Notify(ch, os.Interrupt)
	go func() {
		for {
			select {
			case <-time.After(5 * time.Second):
				err := s.update()
				if err != nil {
					s.srv.Close()
					return
				}
			case <-ch:
				s.srv.Close()
			}
		}
	}()
	s.srv.ListenAndServe()
}
