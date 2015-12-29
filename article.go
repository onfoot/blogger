package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/russross/blackfriday"
)

type PageType string

const (
	Post    PageType = "Post"
	Page             = "Page"
	Snippet          = "Snippet"
)

const (
	DefaultDateFormat string = time.RFC3339
	IFTTTDateFormat			= "January 02, 2006 at 03:04PM"
)

type Article struct {
	Author       string
	DateModified *time.Time
	DateUpdated  *time.Time
	Title        string
	Content      string
	RawContent	string
	Description  string
	Filename     string
	Link         string
	Identifier   string
	Snippet      bool
	Type         PageType
	Draft        bool
	Tags         []string
	AppID        string
	Meta         map[string]string
}

func (a Article) HasTag(aTag string) bool {
	for _, tag := range a.Tags {
		if aTag == tag {
			return true
		}
	}

	return false
}

func (a Article) BasePath() string {
	switch a.Type {
	case Post, Snippet:
		return path.Join(strconv.Itoa(a.DateModified.Year()), fmt.Sprintf("%02d", int(a.DateModified.Month())))
	case Page:
		return ""
	}

	return ""
}

func (a Article) FullPath() string {
	return path.Join(a.BasePath(), a.Filename)
}

type Articles []*Article

func (a Articles) Len() int {
	return len(a)
}

func (a Articles) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a Articles) Less(i, j int) bool {
	left := *(a[i].DateModified)
	right := *(a[j].DateModified)

	return right.Before(left)
}

func (a Article) Print() {
	fmt.Println("---")

	var articleType string

	switch a.Type {
	case Post:
		articleType = "Post"
	case Snippet:
		articleType = "Snippet"
	case Page:
		articleType = "Page"
	}

	if a.Type != Snippet {
		fmt.Printf("title: %s\n", a.Title)
	}

	fmt.Printf("author: %s\n", a.Author)
	fmt.Printf("type: %s\n", articleType)
	fmt.Println("tags: ")
	if a.DateModified != nil {
		fmt.Printf("date: %v\n", a.DateModified.Format(DefaultDateFormat))
	}

	if a.DateUpdated != nil {
		fmt.Printf("date: %v\n", a.DateUpdated.Format(DefaultDateFormat))
	}

	if len(a.AppID) > 0 {
		fmt.Printf("appid: %v\n", a.AppID)
	}

	if a.Meta != nil {

	}

	fmt.Println("---")
	fmt.Println("")
}

func ParseFrontMatter(reader *bufio.Reader) (map[string]string, error) {

	data := make(map[string]string)

	line, lineErr := reader.ReadString('\n')

	if !strings.HasPrefix(line, "---") {
		return data, errors.New("Invalid front matter header")
	}

	for lineErr == nil {
		line, lineErr = reader.ReadString('\n')
		if lineErr != nil {
			continue
		}

		if strings.HasPrefix(line, "---") {
			break
		}

		values := strings.SplitN(line, ":", 2)

		if len(values) < 2 {
			line, lineErr = reader.ReadString('\n')
			continue
		}

		key, value := values[0], values[1]

		key = strings.Trim(key, " \t\r\n")
		value = strings.Trim(value, " \t\r\n")

		data[key] = value
	}

	return data, nil
}

func ReadArticle(reader *bufio.Reader) (Article, error) {
	article := Article{}

	frontMatter, matterErr := ParseFrontMatter(reader)

	if matterErr != nil {
		return article, errors.New("Invalid article header")
	}

	dateModifiedFound := false

	for key, value := range frontMatter {

		if strings.HasPrefix(key, "meta-") {
			if article.Meta == nil {
				article.Meta = make(map[string]string)
			}
			metaName := strings.TrimPrefix(key, "meta-")
			article.Meta[metaName] = value
			continue
		}

		switch key {
		case "title":
			article.Title = value
		case "author":
			article.Author = value
		case "description":
			article.Description = value
		case "link":
			article.Link = value
		case "date":
			dateStr := value
			modTime, timeErr := time.Parse(DefaultDateFormat, dateStr)
			if timeErr == nil {
				article.DateModified = &modTime
				dateModifiedFound = true
				break
			}

			modTime, timeErr = time.Parse(IFTTTDateFormat, dateStr)
			if timeErr == nil {
				article.DateModified = &modTime
				dateModifiedFound = true
				break
			}

			if timeErr != nil {
				log.Fatalf("Could not parse date \"%s\"", dateStr)
			}

		case "updated":
			dateStr := value
			modTime, timeErr := time.Parse(DefaultDateFormat, dateStr)
			if timeErr == nil {
				article.DateUpdated = &modTime
				break
			}

			modTime, timeErr = time.Parse(IFTTTDateFormat, dateStr)
			if timeErr == nil {
				article.DateUpdated = &modTime
				break
			}

			if timeErr != nil {
				log.Fatalf("Could not parse date \"%s\"", dateStr)
			}

		case "appid":
			article.AppID = value
		case "type":
			switch value {
			case "Post":
				article.Type = Post
			case "Page":
				article.Type = Page
			case "Snippet":
				article.Type = Snippet
			}

		case "tags":
			fieldsFunc := func(divider rune) bool {
				return unicode.IsSpace(divider) || divider == ',' || divider == ';'
			}

			for _, tag := range strings.FieldsFunc(value, fieldsFunc) {
				article.Tags = append(article.Tags, strings.ToLower(tag))
			}
		}

	}

	if !dateModifiedFound {
		now := time.Now()
		article.DateModified = &now
	}

	contentBuffer := bytes.NewBufferString("")
	reader.WriteTo(contentBuffer)

	htmlFlags := 0
	htmlFlags |= blackfriday.HTML_USE_SMARTYPANTS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_FRACTIONS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_LATEX_DASHES
	renderer := blackfriday.HtmlRenderer(htmlFlags, "", "")
	extensions := 0
	extensions |= blackfriday.EXTENSION_NO_INTRA_EMPHASIS
	extensions |= blackfriday.EXTENSION_TABLES
	extensions |= blackfriday.EXTENSION_FENCED_CODE
	extensions |= blackfriday.EXTENSION_AUTOLINK
	extensions |= blackfriday.EXTENSION_STRIKETHROUGH
	extensions |= blackfriday.EXTENSION_SPACE_HEADERS
	extensions |= blackfriday.EXTENSION_HEADER_IDS

	md := blackfriday.Markdown(contentBuffer.Bytes(), renderer, extensions)

	article.Content = string(md)

	if len(article.Description) == 0 {
		article.Description = article.Content
	}

	return article, nil
}
