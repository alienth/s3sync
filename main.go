package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alienth/fastlyctl/util"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/fsnotify/fsnotify"
	"github.com/urfave/cli"
)

// Only syncs directories.

// Continuous sync in either direction
// Build manifest and store that remotely.
// option - build manifest every X minutes, or every X events
// Optionally delete things remotely that don't exist locally.
// Exclude/include filters.
// List manifests.
// Restore a directory to a given manifest.
// Sign contents.

// syncer sync /tmp/boo s3://alienthtest/

var noop = false
var recurse = false

func main() {
	app := cli.NewApp()
	app.Name = "syncer"

	app.Flags = []cli.Flag{
		// cli.BoolFlag{
		// 	Name:  "verbose, v",
		// 	Usage: "Print detailed info.",
		// },
	}

	app.Commands = []cli.Command{
		cli.Command{
			Name:      "sync",
			Aliases:   []string{"p"},
			Usage:     "Continuously copy all objects from source to the destination.",
			ArgsUsage: "<SOURCE> <DESTINATION>...",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "noop, n",
					Usage: "Push new config versions, but do not activate.",
				},
				cli.BoolFlag{
					Name:  "recursive, r",
					Usage: "Recurse into directories",
				},
				cli.BoolFlag{
					Name:  "delete, d",
					Usage: "Delete objects in destination that aren't present in the source.",
				},
				cli.BoolFlag{
					Name:  "one-time",
					Usage: "Only sync one-time rather than continuously.",
				},
			},
			Before: func(c *cli.Context) error {
				if !util.IsInteractive() && !c.GlobalBool("assume-yes") {
					return cli.NewExitError(util.ErrNonInteractive.Error(), -1)
				}
				if c.Bool("noop") {
					log.Println("!!! Running in no-op mode.")
					noop = true
				}
				if c.Bool("recurse") {
					recurse = true
				}
				return nil
			},
			Action: sync,
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Printf("Error starting app: %s", err)
	}

}

func sync(c *cli.Context) error {
	source, destination, err := getLocations(c)
	if err != nil {
		return err
	}

	source.buildManifest()
	source.listManifest()
	destination.buildManifest()
	destination.listManifest()

	// /tmp/bar/        -> s3://alienthtest/
	// /tmp/bar/foo     -> s3://alienthtest/foo
	// /tmp/bar/foo/bar -> s3://alienthtest/foo/bar

	oneTimeSync(c, source, destination)

	source.Watch()
	return nil
}

func (l *location) Watch() {
	watcher, _ := fsnotify.NewWatcher()
	watcher.Add(l.Path)
	if recurse {
		files, err := ioutil.ReadDir(l.Path)
		if err != nil {
			log.Fatal(err)
		}

		for _, f := range files {
			if f.IsDir() {
				watcher.Add(l.Path + "/" + f.Name())
			}
		}
	}

	for {
		select {
		case event := <-watcher.Events:
			fi, _ := os.Lstat(event.Name)
			if fi.IsDir() {
				if recurse {
					watcher.Add(event.Name)
				}
				continue
			}
			key := strings.TrimPrefix(event.Name, l.Path)
			l.handleEvent(key, event)
		}
	}
}

// Takes in a cli context, parses the args, and returns the source and destination locations
func getLocations(c *cli.Context) (*location, *location, error) {
	if len(c.Args()) != 2 {
		log.Fatal("must pass 2 args")
	}

	results := make([]location, 2)

	for i, param := range c.Args() {
		loc := location{Path: param}
		if i == 0 {
			loc.Type = Source
		} else if i == 1 {
			loc.Type = Destination
		}
		u, err := url.Parse(param)
		if err != nil {
			return nil, nil, err
		}
		if u.Scheme == "" {
			// Should be a directory?
			loc.Path = param
			if loc.Service, err = os.Lstat(param); err != nil {
				log.Fatal(err)
			}
			results[i] = loc

		} else if u.Scheme == "s3" {
			sess, err := session.NewSessionWithOptions(session.Options{SharedConfigState: session.SharedConfigEnable})
			if err != nil {
				log.Fatal(err)
			}
			svc := s3.New(sess)
			loc.Bucket = u.Host
			loc.Service = svc
			results[i] = loc
		} else {
			return nil, nil, fmt.Errorf("Unsupported location type \"%s\" for location %s\n", u.Scheme, param)
		}
	}

	results[0].Destination = &results[1]

	return &results[0], &results[1], nil
}

type LocationType int

const (
	Source LocationType = iota
	Destination
)

type location struct {
	Service     interface{}
	Bucket      string
	Path        string          // the base path which we manage objects from
	Manifest    map[string]file // the key is the object relative to the location's Path
	Type        LocationType
	Destination *location
}

// How do we strip the source's path when writing things to a destination?
// Takes in an fsnotify event and dispatches the appropriate location action depending on the event type.
// key is the relative path of the object.
func (l *location) handleEvent(key string, event fsnotify.Event) {
	if l.Type == Source {
		switch event.Op {
		case fsnotify.Create:
			f := constructFile(event)
			l.Manifest[key] = f
			l.Destination.Put(key, f)
		case fsnotify.Write:
			f := constructFile(event)
			l.Destination.Put(key, f)
		case fsnotify.Remove:
			l.Destination.Delete(key)
			delete(l.Manifest, key)
		}
	} else {
		log.Fatal("we don't do this")
	}
}

// Takes in a file-like object and returns a file.
func constructFile(input interface{}) file {
	var f file
	switch i := input.(type) {
	case fsnotify.Event:
		var err error
		f.Name = filepath.Base(i.Name)
		f.Path = filepath.Dir(i.Name)
		f.Object, err = os.Stat(i.Name)
		if err != nil {
			log.Fatal(err)
		}
		return f
	case *s3.Object:
		s3Key := *i.Key
		f.Name = filepath.Base(s3Key)
		f.Path = filepath.Dir(s3Key)
		f.Size = int(*i.Size)
		f.Object = i
		f.LastModified = *i.LastModified
	}
	return f
}

func (l *location) buildManifest() {
	l.Manifest = make(map[string]file)
	switch svc := l.Service.(type) {
	case *s3.S3:
		foo := s3.ListObjectsV2Input{}
		foo.Bucket = aws.String(l.Bucket)
		foo.Prefix = aws.String(l.Path)
		f := func(list *s3.ListObjectsV2Output, lastPage bool) bool {
			for _, o := range list.Contents {
				f := constructFile(o)
				key := "/" + strings.TrimPrefix(*o.Key, l.Path)
				l.Manifest[key] = f
			}

			// Fetch all pages
			return true
		}
		var err error
		if err = svc.ListObjectsV2Pages(&foo, f); err != nil {
			log.Fatal(err)
		}
	case os.FileInfo:
		l.buildDirManifest(l.Path)
	default:
		log.Fatal("unknown type")
	}
}

func (l *location) buildDirManifest(dir string) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	// replace this with a filepath.Walk call
	for _, fi := range files {
		if fi.IsDir() {
			l.buildDirManifest(dir + "/" + fi.Name())
		} else {
			var f file
			key := dir + "/" + fi.Name()
			key = filepath.Clean(strings.TrimPrefix(key, l.Path))
			f.Name = fi.Name()
			f.Size = int(fi.Size())
			f.Object = fi
			f.Path = dir
			f.LastModified = fi.ModTime()
			l.Manifest[key] = f
		}
	}
}

func (l *location) listManifest() {
	for key, f := range l.Manifest {
		log.Println(key, f.Name, f.Size, f.LastModified)
	}
}

// key is the destination relative to the location's Path.
// f is the thing we're Puting.
func (l *location) Put(key string, f file) {
	reader := f.Open()
	switch svc := l.Service.(type) {
	case *s3.S3:
		foo := s3.PutObjectInput{
			Bucket: aws.String(l.Bucket),
			Body:   reader,
			Key:    aws.String(key),
		}
		_, err := svc.PutObject(&foo)
		if err != nil {
			log.Fatal(err)
		}
		l.Manifest[key] = f
	case os.FileInfo:
		out, err := os.Create(l.Path + "/" + key)
		defer out.Close()
		if err != nil {
			log.Fatal("error creating:", err)
		}
		if _, err = io.Copy(out, reader); err != nil {
			log.Fatal(err)
		}
		out.Sync()
	default:
		log.Fatal("can't handle")
	}
}

func (l *location) Delete(key string) {
	switch svc := l.Service.(type) {
	case *s3.S3:
		foo := s3.DeleteObjectInput{
			Bucket: aws.String(l.Bucket),
			Key:    aws.String(key)}

		_, err := svc.DeleteObject(&foo)
		if err != nil {
			log.Fatal(err)
		}
	case os.FileInfo:
		if err := os.Remove(l.Path + "/" + key); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("can't handle")
	}

	delete(l.Manifest, key)
}

func oneTimeSync(c *cli.Context, source, destination *location) {
	// for each object in the source, push it to the destination
	for key, f := range source.Manifest {
		if destF, ok := destination.Manifest[key]; !ok {
			log.Printf("pushing missing %s to destination.", key)
			destination.Put(key, f)
		} else {
			if f.Size != destF.Size {
				log.Printf("pushing mismatched %s to destination.", key)
				destination.Put(key, f)
			}
		}
	}

	// for each object in the destination not in the source, delete it from the destination
	if c.Bool("delete") {
		for key, _ := range destination.Manifest {
			if _, ok := source.Manifest[key]; !ok {
				log.Printf("deleting %s from destination.", key)
				destination.Delete(key)
			}
		}
	}
}

type file struct {
	Name         string // The basename of a file
	Path         string // The absolute directory of a file
	LastModified time.Time
	Size         int
	Object       interface{}
}

func (f *file) Open() io.ReadSeeker {
	switch o := f.Object.(type) {
	case os.FileInfo:
		_ = o
		// TODO - This never gets closed.
		r, err := os.Open(f.Path + "/" + f.Name)
		if err != nil {
			log.Fatal("error opening file:", err)
		}
		return r
	}
	return nil
}
