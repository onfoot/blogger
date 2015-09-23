package main

import (
	"bufio"
	"bytes"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"text/template"
	"time"
)

var blogTitle = flag.String("title", "blog", "Blog title")
var destinationExt = flag.String("extension", "", "Destination file extension")
var postsPath = flag.String("posts", "posts", "Posts directory, comma separated for multiple directories")
var templatesPath = flag.String("templates", "templates", "Templates directory")
var destinationPath = flag.String("destination", "destination", "Destination directory")
var siteRoot = flag.String("root", "/", "Site root path")
var templatePrint = flag.String("print", "", "Print out a template for a snippet or a blog post")
var templateAuthor = flag.String("author", "", "Set a default post author")

const templateFileName = "template.html"
const rssTemplateFileName = "rsstemplate.html"

func main() {

	flag.Parse()

	if *templatePrint != "" {
		var article Article
		now := time.Now().Add(15 * time.Minute)
		article.DateModified = &now

		if *templateAuthor != "" {
			article.Author = *templateAuthor
		}

		switch *templatePrint {
		case "post":
			article.Title = "Blog post"
			article.Type = Post
			break
		case "snippet":
			article.Type = Snippet
			break
		default:
			log.Fatal("post and snippet are the only allowed parameters for -print")
		}

		article.Print()

		return
	}

	log.Printf("Blog title is %s", *blogTitle)

	destinationDir, destinationDirErr := os.Open(*destinationPath)

	if destinationDirErr != nil {
		log.Fatal("Destination directory could not be open: ", destinationDirErr)
	}

	defer destinationDir.Close()

	funcMap := template.FuncMap{
		"longDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("Monday, _2 January 2006, 15:04") },
		"snippetDate":  func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2 2006, 15:04") },
		"shortDate":    func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2, 2006") },
		"atomDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("2006-01-02T15:04:05Z07:00") },
		"Snippet":      func(args ...interface{}) bool { return args[0].(*Article).Type == Snippet },
		"last":         func(index, count int) bool { return index == count-1 },
		"tagIndexName": func(tag string) string { return "tag-" + tag + *destinationExt },
		"path": func(article Article) string {
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

		files, filesErr := ioutil.ReadDir(postDir)
		if filesErr != nil {
			log.Fatal("Source directory could not be read")
		}

		for _, file := range files {
			ext := path.Ext(file.Name())

			name := strings.TrimSuffix(path.Base(file.Name()), ext)

			if file.IsDir() || (ext != ".markdown" && ext != ".md" && ext != ".txt") {
				continue
			}

			sourceFiles = append(sourceFiles, PostFile{Name: name, Extension: ext, Path: path.Join(postDir, file.Name())})
		}
	}

	var articles Articles

	var indexArticles Articles
	var feedArticles Articles
	var snippetArticles Articles

	for _, sourceFile := range sourceFiles {

		fileContent, fileError := ioutil.ReadFile(sourceFile.Path)

		if fileError != nil {
			log.Fatal("Read file error", fileError)
		}

		sourceBuffer := bytes.NewBuffer(fileContent)

		mdReader := bufio.NewReader(sourceBuffer)

		article, readErr := ReadArticle(mdReader)

		if readErr != nil {
			log.Fatal("Bad article")
		}

		article.Filename = sourceFile.Name + *destinationExt

		article.Description = strings.Replace(article.Description, "$SITEROOT", *siteRoot, -1)
		article.Content = strings.Replace(article.Content, "$SITEROOT", *siteRoot, -1)

		if article.DateModified == nil {
			article.DateModified = new(time.Time)
		}

		article.Identifier = sourceFile.Name

		if strings.HasSuffix(sourceFile.Name, "draft") {
			article.Draft = true
		}

		articles = append(articles, &article)

		if article.Type == Page {
			continue
		}

		if article.Draft {
			continue
		}

		if article.Type == Post {
			feedArticles = append(feedArticles, &article)
		}

		if article.Type == Snippet {
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

	mainTemplate.Execute(indexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": *siteRoot, "Articles": indexArticles, "CreatedTime": now})
	mainRssTemplate.Execute(rssIndexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": *siteRoot, "File": "index.xml", "Articles": feedArticles, "CreatedTime": &now})
	mainRssTemplate.Execute(snippetrssIndexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": *siteRoot, "File": "snippets.xml", "Articles": snippetArticles, "CreatedTime": &now})

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
			log.Println(writeErr)
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
		var tagArticles Articles

		for _, article := range indexArticles {

			if !article.HasTag(tag) {
				continue
			}

			tagArticles = append(tagArticles, article)
		}

		mainTemplate.Execute(tagIndexBuffer, map[string]interface{}{"Articles": tagArticles, "Title": "Tag: " + tag + " – " + *blogTitle, "Home": false, "Root": *siteRoot})

		tagIndexFileName := path.Join(destinationDir.Name(), "tag-"+tag+*destinationExt)
		ioutil.WriteFile(tagIndexFileName, tagIndexBuffer.Bytes(), os.ModePerm)
	}
}
