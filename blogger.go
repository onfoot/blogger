package main

import (
	"bufio"
	"bytes"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"macbirdie.net/blogger/post"

	"github.com/russross/blackfriday"

	"gopkg.in/fsnotify.v1"
)

var blogTitle = flag.String("title", "blog", "Blog title")
var destinationExt = flag.String("extension", "", "Destination file extension")
var postsPath = flag.String("posts", "posts", "Posts directory, comma separated for multiple directories")
var templatesPath = flag.String("templates", "templates", "Templates directory")
var destinationPath = flag.String("destination", "destination", "Destination directory")
var siteRoot = flag.String("root", "/", "Site root path")
var templatePrint = flag.String("print", "", "Print out a template for a snippet, blog post or a page")
var templateAuthor = flag.String("author", "", "Set a default post author")
var listen = flag.Bool("listen", false, "Listen to changes in post directories and regenerate")

const templateFileName = "template.html"
const rssTemplateFileName = "rsstemplate.html"

var postExtensions = []string{".md", ".markdown", ".txt"}

func containsString(haystack []string, needle string) bool {
	for _, hay := range haystack {
		if hay == needle {
			return true
		}
	}

	return false
}

func generate() {

	log.Printf("Generating blog: %s", *blogTitle)

	destinationDir, destinationDirErr := os.Open(*destinationPath)

	if destinationDirErr != nil {
		log.Fatal("Destination directory could not be opened: ", destinationDirErr)
	}

	defer destinationDir.Close()

	funcMap := template.FuncMap{
		"longDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("Monday, _2 January 2006, 15:04") },
		"snippetDate":  func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2 2006, 15:04") },
		"shortDate":    func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2, 2006") },
		"atomDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("2006-01-02T15:04:05Z07:00") },
		"Snippet":      func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Snippet },
		"Post":         func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Post },
		"Page":         func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Page },
		"last":         func(index, count int) bool { return index == count-1 },
		"tagIndexName": func(tag string) string { return "tag-" + tag + *destinationExt },
		"path": func(article post.Article) string {
			return article.FullPath()
		},
	}

	mainTemplate := template.Must(template.New("template.html").Funcs(funcMap).ParseFiles(path.Join(*templatesPath, templateFileName)))
	mainRssTemplate := template.Must(template.New("rsstemplate.html").Funcs(funcMap).ParseFiles(path.Join(*templatesPath, rssTemplateFileName)))

	now := time.Now()

	type PostFile struct {
		Name      string
		Extension string
		Path      string
	}

	sourceFiles := []PostFile{}

	for _, postDir := range strings.Split(*postsPath, ",") {

		walkFunc := func(filepath string, info os.FileInfo, err error) error {
			if err != nil {
				log.Fatalf("Post directory %q not found", filepath)
			}

			if info.IsDir() {
				return nil
			}

			filename := path.Base(info.Name())
			ext := path.Ext(filename)

			if !containsString(postExtensions, ext) {
				return nil
			}

			for {
				filename = strings.TrimSuffix(filename, ext)
				ext = path.Ext(filename)

				if !containsString(postExtensions, ext) {
					break
				}
			}

			sourceFiles = append(sourceFiles, PostFile{Name: filename, Extension: ext, Path: filepath})

			return nil
		}

		filepath.Walk(postDir, walkFunc)
	}

	var articles, indexArticles, feedArticles, snippetArticles post.Articles

	htmlFlags := 0
	htmlFlags |= blackfriday.HTML_USE_SMARTYPANTS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_FRACTIONS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_LATEX_DASHES

	var rendererParameters blackfriday.HtmlRendererParameters

	htmlPrefix := *siteRoot
	htmlPrefix = strings.TrimSuffix(htmlPrefix, "/")
	rendererParameters.AbsolutePrefix = htmlPrefix

	log.Println("Using prefix", htmlPrefix)
	renderer := blackfriday.HtmlRendererWithParameters(htmlFlags, "", "", rendererParameters)
	extensions := 0
	extensions |= blackfriday.EXTENSION_NO_INTRA_EMPHASIS
	extensions |= blackfriday.EXTENSION_TABLES
	extensions |= blackfriday.EXTENSION_FENCED_CODE
	extensions |= blackfriday.EXTENSION_AUTOLINK
	extensions |= blackfriday.EXTENSION_STRIKETHROUGH
	extensions |= blackfriday.EXTENSION_SPACE_HEADERS
	extensions |= blackfriday.EXTENSION_HEADER_IDS
	extensions |= blackfriday.EXTENSION_FOOTNOTES

	for _, sourceFile := range sourceFiles {

		file, fileError := os.Open(sourceFile.Path)

		if fileError != nil {
			log.Printf("Skipping %v due to error: %v", sourceFile.Path, fileError)
			continue
		}

		defer file.Close()

		article, readErr := post.ReadArticle(bufio.NewReader(file))

		if readErr != nil {
			log.Printf("Skipping file %v due to parse error: %v", sourceFile.Path, readErr)
			continue
		}

		md := blackfriday.Markdown(article.RawContent, renderer, extensions)

		article.Content = string(md)

		article.Filename = sourceFile.Name + *destinationExt

		if article.DateModified == nil {
			article.DateModified = new(time.Time)
		}

		article.Identifier = sourceFile.Name

		articles = append(articles, &article)

		if article.Type == post.Page {
			continue
		}

		if article.Draft {
			continue
		}

		if article.Type == post.Post {
			feedArticles = append(feedArticles, &article)
		}

		if article.Type == post.Snippet {
			snippetArticles = append(snippetArticles, &article)
		}

		indexArticles = append(indexArticles, &article)
	}

	tags := map[string]bool{}

	sort.Sort(articles)
	sort.Sort(indexArticles)
	sort.Sort(feedArticles)
	sort.Sort(snippetArticles)

	indexBuffer := bytes.NewBufferString("")
	rssIndexBuffer := bytes.NewBufferString("")
	snippetrssIndexBuffer := bytes.NewBufferString("")

	mainTemplate.Execute(indexBuffer, map[string]interface{}{
		"Title":       blogTitle,
		"Home":        true,
		"Root":        *siteRoot,
		"Articles":    indexArticles,
		"CreatedTime": now,
	})

	mainRssTemplate.Execute(rssIndexBuffer, map[string]interface{}{
		"Title":       blogTitle,
		"Home":        true,
		"Root":        *siteRoot,
		"File":        "index.xml",
		"Articles":    feedArticles,
		"CreatedTime": &now,
	})

	mainRssTemplate.Execute(snippetrssIndexBuffer, map[string]interface{}{
		"Title":       blogTitle,
		"Home":        true,
		"Root":        *siteRoot,
		"File":        "snippets.xml",
		"Articles":    snippetArticles,
		"CreatedTime": &now,
	})

	for _, article := range articles {

		destFileBuffer := bytes.NewBufferString("")

		mainTemplate.Execute(destFileBuffer, map[string]interface{}{
			"BlogTitle": blogTitle,
			"Article":   article,
			"Title":     string(article.Title + " – " + *blogTitle),
			"Home":      false,
			"Root":      *siteRoot,
		})

		for _, tag := range article.Tags {
			tags[tag] = true
		}

		destinationFileName := path.Join(destinationDir.Name(), article.FullPath())

		os.MkdirAll(path.Join(destinationDir.Name(), article.BasePath()), os.ModePerm)

		writeErr := ioutil.WriteFile(destinationFileName, destFileBuffer.Bytes(), os.ModePerm)

		if writeErr != nil {
			log.Printf("Could not write file %v due to error: %v", destinationFileName, writeErr)
		}
	}

	indexFileName := path.Join(destinationDir.Name(), "index.html")
	rssIndexFileName := path.Join(destinationDir.Name(), "index.xml")
	snippetIndexFileName := path.Join(destinationDir.Name(), "snippets.xml")

	ioutil.WriteFile(indexFileName, indexBuffer.Bytes(), os.ModePerm)
	ioutil.WriteFile(rssIndexFileName, rssIndexBuffer.Bytes(), os.ModePerm)
	ioutil.WriteFile(snippetIndexFileName, snippetrssIndexBuffer.Bytes(), os.ModePerm)

	for tag := range tags {

		tagIndexBuffer := bytes.NewBufferString("")
		var tagArticles post.Articles

		for _, article := range indexArticles {

			if !article.HasTag(tag) {
				continue
			}

			tagArticles = append(tagArticles, article)
		}

		mainTemplate.Execute(tagIndexBuffer, map[string]interface{}{
			"Articles": tagArticles,
			"Title":    "Tag: " + tag + " – " + *blogTitle,
			"Home":     false,
			"Root":     *siteRoot,
		})

		tagIndexFileName := path.Join(destinationDir.Name(), "tag-"+tag+*destinationExt)
		ioutil.WriteFile(tagIndexFileName, tagIndexBuffer.Bytes(), os.ModePerm)
	}
}

func watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal("Couldn't watch the post directories")
	}
	defer watcher.Close()

	watcherDone := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("Modified file: ", event.Name)
					generate()
				}
			case err := <-watcher.Errors:
				log.Println("Got error:", err)
			}
		}
	}()

	var watchedDirs []string

	for _, postDir := range strings.Split(*postsPath, ",") {

		walkFunc := func(filepath string, info os.FileInfo, err error) error {
			if !info.IsDir() {
				return nil
			}

			watchedDirs = append(watchedDirs, filepath)

			return nil
		}

		filepath.Walk(postDir, walkFunc)
	}

	watchedDirs = append(watchedDirs, *templatesPath)

	for _, watchedDir := range watchedDirs {
		watcher.Add(watchedDir)
	}

	log.Printf("Listening to changes in directories: %s…", strings.Join(watchedDirs, ", "))

	<-watcherDone
}

func main() {
	flag.Parse()

	if *templatePrint != "" {
		var article post.Article
		now := time.Now().Add(15 * time.Minute)
		article.DateModified = &now

		article.Draft = true

		if *templateAuthor != "" {
			article.Author = *templateAuthor
		}

		switch *templatePrint {
		case "page":
			article.Title = "Hello world"
			article.Type = post.Page
			break
		case "post":
			article.Title = "Blog post"
			article.Type = post.Post
			break
		case "snippet":
			article.Type = post.Snippet
			break
		default:
			log.Fatal("post, snippet and page are the only allowed parameters for -print")
		}

		article.Print()

		return
	}

	generate()

	if *listen {
		watch()
	}
}
