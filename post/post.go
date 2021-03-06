package post

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// PageType is a convenience type alias for page type
type PageType string

const (
	// Post - A regular blog article
	Post PageType = "Post"
	// Page - A site-wide page
	Page = "Page"
	// Snippet - Twitter-like short blog post
	Snippet = "Snippet"
)

const (
	// DefaultDateFormat is the default format used in posts and templates
	DefaultDateFormat string = time.RFC3339
	// IFTTTDateFormat is a date format IFTTT uses for recipe ingredients in actions
	IFTTTDateFormat = "January 02, 2006 at 03:04PM"
)

type Tag struct {
	Name   string
	OriginalName string
	Hidden bool
}

func MakeTag(tag string) Tag {
	prepared := strings.ToLower(tag)
	trimmed := strings.TrimPrefix(prepared, "-")
	hidden := trimmed != prepared

	return Tag{Name: trimmed, OriginalName: tag, Hidden: hidden}
}

func (t Tag) FileName() string {
	if t.Hidden {
		return strings.Join([]string{"_", t.Name}, "")
		} else {
		return t.Name
		}
}

// Article represents a blogger post, page or a snippet. Contains information useful for blog publishing.
type Article struct {
	Author       string
	DateModified *time.Time
	DateUpdated  *time.Time
	Title        string
	Content      string
	RawContent   []byte
	Description  string
	Filename     string
	Link         string
	Identifier   string
	Snippet      bool
	Type         PageType
	Draft        bool
	Tags         []Tag
	AppID        string
	Meta         map[string]string
}

// HasTag checks if the given article contains a certain tag
func (a Article) HasTag(aTag string) bool {
	for _, tag := range a.Tags {
		if aTag == tag.Name {
			return true
		}
	}

	return false
}

func (a Article) VisibleTags() []Tag {
	tags := make([]Tag, 0)
	for _, tag := range a.Tags {
		if !tag.Hidden {
			tags = append(tags, tag)
		}
	}
	return tags
}

// BasePath returns a base path for the given article, relative to blog root path
func (a Article) BasePath() string {
	switch a.Type {
	case Post, Snippet:
		if a.Draft {
			return "drafts"
		}

	case Page:
		return ""
	}

	return path.Join(strconv.Itoa(a.DateModified.Year()), fmt.Sprintf("%02d", int(a.DateModified.Month())))
}

// FullPath combines BasePath with articles file name
func (a Article) FullPath() string {
	return path.Join(a.BasePath(), a.Filename)
}

// Articles is an convenience type alias for article slice
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

// Print sends an article header in plain text to standard output
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

	if a.Draft {
		fmt.Printf("draft: true\n")
	}

	if a.Meta != nil {
	}

	fmt.Println("---")
	fmt.Println("")
}

// ParseFrontMatter reads the front matter-type article header
func ParseFrontMatter(reader *bufio.Reader) (map[string]string, error) {

	data := make(map[string]string)

	line, lineErr := reader.ReadString('\n')

	if !strings.HasPrefix(line, "---") {
		return data, errors.New("Invalid front matter header")
	}

	for {
		line, lineErr = reader.ReadString('\n')
		if lineErr != nil {
			break
		}

		if strings.HasPrefix(line, "---") {
			break
		}

		values := strings.SplitN(line, ":", 2)

		if len(values) < 2 {
			continue
		}

		key, value := values[0], values[1]

		key = strings.Trim(key, " \t\r\n")
		value = strings.Trim(value, " \t\r\n")

		data[key] = value
	}

	return data, nil
}

// ReadArticle returns an article read from a Reader
func ReadArticle(reader *bufio.Reader) (Article, error) {
	article := Article{}

	frontMatter, matterErr := ParseFrontMatter(reader)

	if matterErr != nil {
		return article, errors.New("Invalid article header")
	}

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
				break
			}

			modTime, timeErr = time.Parse(IFTTTDateFormat, dateStr)
			if timeErr == nil {
				article.DateModified = &modTime
				break
			}

			if timeErr != nil {
				return article, timeErr
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
				return article, timeErr
			}

		case "appid":
			article.AppID = value
		case "draft":
			article.Draft = (value == "true")
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
				article.Tags = append(article.Tags, MakeTag(tag))
			}
		}
	}

	if article.DateModified == nil {
		now := time.Now()
		article.DateModified = &now
	}

	var contentBuffer bytes.Buffer
	reader.WriteTo(&contentBuffer)

	article.RawContent = contentBuffer.Bytes()

	return article, nil
}
