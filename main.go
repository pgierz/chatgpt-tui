package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/mitchellh/go-homedir"
	"github.com/pkoukk/tiktoken-go"
	"github.com/rivo/tview"
	"github.com/tidwall/buntdb"
)

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"

	systemMessage = "You are ChatGPT, a large language model trained by OpenAI. Answer as concisely as possible."

	prefixSuggestTitle = "suggest me a short title for "

	pageMain        = "main"
	pageEditTitle   = "editTitle"
	pageDeleteTitle = "deleteTitle"

	buttonCancel = "Cancel"
	buttonDelete = "Delete"

	maxTokens = 4097
)

var errTimeout = errors.New("timeout")

type Conversation struct {
	Time     int64     `json:"time"`
	Messages []Message `json:"messages"`
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Please set `OPENAI_API_KEY` environment variable. You can find your API key at https://platform.openai.com/account/api-keys.")
		os.Exit(1)
	}

	home, err := homedir.Dir()
	if err != nil {
		log.Panic(err)
	}

	dbPath := filepath.Join(home, ".chatgpt")
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		log.Panic(err)
	}

	dbFile := filepath.Join(dbPath, "history.db")
	f, err := os.OpenFile(dbFile, os.O_RDWR|os.O_CREATE, 0640)
	if err != nil {
		log.Panic(err)
	}
	defer f.Close()

	if err := flock(f, 1*time.Second); err != nil {
		if errors.Is(err, errTimeout) {
			fmt.Println("Another process is already running.")
		} else {
			fmt.Println(err)
		}
		return
	}

	db, err := buntdb.Open(dbFile)
	if err != nil {
		log.Panic(err)
	}
	defer db.Close()
	db.CreateIndex("time", "*", buntdb.IndexJSON("time"))

	textArea := tview.NewTextArea()
	textArea.SetTitle("Question").SetBorder(true)

	list := tview.NewList()
	list.SetTitle("History").SetBorder(true)

	// tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	app := tview.NewApplication()
	textView := tview.NewTextView().
		SetChangedFunc(func() {
			app.Draw()
		}).
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true)
	textView.SetTitle("Conversation").SetBorder(true)
	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyESC:
			app.SetFocus(list)
		case tcell.KeyEnter:
			app.SetFocus(textArea)
		}
		return event
	})

	var (
		m         = make(map[string]*Conversation)
		isNewChat = true
	)
	list.SetSelectedFocusOnly(true)
	db.View(func(tx *buntdb.Tx) error {
		err := tx.Descend("time", func(key, value string) bool {
			var c *Conversation
			if err := json.Unmarshal([]byte(value), &c); err == nil {
				m[key] = c

				list.AddItem(key, "", rune(0), func() {
					textView.SetText(toConversation(c.Messages))
				})
			}
			return true
		})
		return err
	})

	list.SetChangedFunc(func(index int, title string, secondaryText string, shortcut rune) {
		if c, ok := m[title]; ok {
			textView.SetText(toConversation(c.Messages))
		}
	})
	list.SetSelectedFunc(func(index int, title string, secondaryText string, shortcut rune) {
		list.SetSelectedFocusOnly(false)
		if c, ok := m[title]; ok {
			textView.SetText(toConversation(c.Messages))
		}

		textView.ScrollToEnd()
		app.SetFocus(textArea)
	})

	pages := tview.NewPages()
	editTitleInputField := tview.NewInputField().
		SetFieldWidth(40).
		SetAcceptanceFunc(tview.InputFieldMaxLength(40))

	deleteTitleModal := tview.NewModal()
	deleteTitleModal.AddButtons([]string{buttonCancel, buttonDelete})

	modal := func(p tview.Primitive, currentRow int) tview.Primitive {
		return tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(tview.NewFlex().SetDirection(tview.FlexColumn).
				AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
					AddItem(nil, 4+(currentRow*2), 1, false).
					AddItem(p, 1, 1, true).
					AddItem(nil, 0, 1, false), 0, 1, true).
				AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
					AddItem(nil, 0, 1, false).
					AddItem(nil, 5, 1, false), 0, 3, false), 0, 1, true).
			AddItem(nil, 1, 1, false)
	}

	searchInputField := tview.NewInputField()
	searchInputField.SetTitle("Search")
	searchInputField.
		SetFieldWidth(50).
		SetAcceptanceFunc(tview.InputFieldMaxLength(50))
	searchInputField.SetBorder(true)
	searchInputField.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			titles := make([]string, 0, len(m))
			db.View(func(tx *buntdb.Tx) error {
				err := tx.Descend("time", func(key, value string) bool {
					titles = append(titles, key)
					return true
				})
				return err
			})

			text := searchInputField.GetText()
			if text != "" {
				idx := make(index)
				idx.add(titles)
				r := idx.search(text)
				list.Clear()
				for _, i := range r {
					list.AddItem(titles[i], "", rune(0), func() {
						if c, ok := m[titles[i]]; ok {
							textView.SetText(toConversation(c.Messages))
						}
					})
				}
			} else {
				list.Clear()
				for i := range titles {
					list.AddItem(titles[i], "", rune(0), func() {
						if c, ok := m[titles[i]]; ok {
							textView.SetText(toConversation(c.Messages))
						}
					})
				}
			}
			if list.GetItemCount() > 0 {
				app.SetFocus(list)
			}
		}
	})

	var hiddenItemCount int
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyESC:
			app.SetFocus(searchInputField)
		}

		_, _, _, height := list.GetInnerRect()

		switch event.Rune() {
		case 'j':
			if list.GetCurrentItem() < list.GetItemCount() {
				list.SetCurrentItem(list.GetCurrentItem() + 1)
			}

			if list.GetCurrentItem() >= height/2 {
				hiddenItemCount = list.GetCurrentItem() + 1 - (height / 2)
			}
		case 'k':
			if list.GetCurrentItem() > 0 {
				list.SetCurrentItem(list.GetCurrentItem() - 1)
			}

			if list.GetCurrentItem()+1 == hiddenItemCount {
				hiddenItemCount--
			}
		case 'e':
			currentIndex := list.GetCurrentItem()
			currentTitle, _ := list.GetItemText(currentIndex)
			editTitleInputField.
				SetText(currentTitle).
				SetDoneFunc(func(key tcell.Key) {
					switch key {
					case tcell.KeyESC:
						pages.HidePage(pageEditTitle)
						app.SetFocus(list)
					case tcell.KeyEnter:
						newTitle := editTitleInputField.GetText()
						if newTitle != currentTitle {
							c, _ := json.Marshal(m[currentTitle])
							if err == nil {
								db.Update(func(tx *buntdb.Tx) error {
									_, _, err := tx.Set(newTitle, string(c), nil)
									if err != nil {
										return err
									}

									tx.Delete(currentTitle)

									m[newTitle] = m[currentTitle]
									delete(m, currentTitle)

									list.RemoveItem(currentIndex)
									list.InsertItem(currentIndex, newTitle, "", rune(0), nil)
									list.SetCurrentItem(currentIndex)

									return nil
								})
							}
						}
						pages.HidePage(pageEditTitle)
						app.SetFocus(list)
					}
				}).
				SetBorder(false)
			pages.AddPage(pageEditTitle, modal(editTitleInputField, list.GetCurrentItem()-hiddenItemCount), true, false)
			pages.ShowPage(pageEditTitle)
		case 'd':
			currentIndex := list.GetCurrentItem()
			currentTitle, _ := list.GetItemText(currentIndex)

			deleteTitleModal.SetText(fmt.Sprintf("Are you sure you want to delete \"%s\"?", currentTitle)).
				SetFocus(0).
				SetDoneFunc(func(buttonIndex int, buttonLabel string) {
					switch buttonLabel {
					case buttonCancel:
						pages.HidePage(pageDeleteTitle)
						app.SetFocus(list)

					case buttonDelete:
						list.RemoveItem(currentIndex)

						if list.GetItemCount() == 0 {
							textView.Clear()
							list.SetCurrentItem(-1)
							app.SetFocus(textArea)
						}

						db.Update(func(tx *buntdb.Tx) error {
							_, err := tx.Delete(currentTitle)
							return err
						})
						delete(m, currentTitle)

						pages.HidePage(pageDeleteTitle)
						if list.GetItemCount() > 0 {
							app.SetFocus(list)
						} else {
							app.SetFocus(textArea)
						}
					}
				}).
				SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					switch event.Key() {
					case tcell.KeyESC:
						pages.HidePage(pageDeleteTitle)
						app.SetFocus(list)
					}
					return event
				})
			pages.ShowPage(pageDeleteTitle)
		}

		return event
	})

	textArea.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyESC:
			if textView.GetText(false) != "" || !isNewChat {
				app.SetFocus(textView)
			}
		case tcell.KeyEnter:
			content := textArea.GetText()
			if strings.TrimSpace(content) == "" {
				return nil
			}
			textArea.SetText("", false)
			textArea.SetDisabled(true)

			titleCh := make(chan string)
			messages := make([]Message, 0)
			if textView.GetText(false) == "" {
				messages = append(messages, Message{
					Role:    roleSystem,
					Content: systemMessage,
				})

				go func() {
					resp, err := createChatCompletion([]Message{
						{
							Role:    roleUser,
							Content: prefixSuggestTitle + content,
						},
					}, false)
					if err != nil {
						log.Panic(err)
					}
					defer resp.Body.Close()

					body, err := io.ReadAll(resp.Body)
					if err != nil {
						log.Panic(err)
					}

					var titleResp *Response
					if err := json.Unmarshal(body, &titleResp); err == nil {
						titleCh <- titleResp.Choices[0].Message.Content
					}
				}()
			} else {
				isNewChat = false

				title, _ := list.GetItemText(list.GetCurrentItem())
				if c, ok := m[title]; ok {
					messages = c.Messages
				}

				textView.ScrollToEnd()
				fmt.Fprintf(textView, "\n\n")
			}

			messages = append(messages, Message{
				Role:    roleUser,
				Content: content,
			})

			numTokens, err := NumTokensFromMessages(messages, gpt3Dot5Turbo)
			if err != nil {
				log.Println(err)
				return nil
			}

			if numTokens > maxTokens {
				isNewChat = true
				title, _ := list.GetItemText(list.GetCurrentItem())
				go func() {
					titleCh <- addSuffixNumber(title)
				}()

				messages = []Message{
					{
						Role:    roleSystem,
						Content: systemMessage,
					},
					{
						Role:    roleUser,
						Content: fmt.Sprintf("%s: %s", title, content),
					},
				}

				textView.Clear()
			}

			fmt.Fprintln(textView, "[red::]You:[-]")
			fmt.Fprintf(textView, "%s\n\n", content)

			respCh := make(chan string)
			errCh := make(chan error, 1)
			go func() {
				resp, err := createChatCompletion(messages, true)
				if err != nil {
					errCh <- err
				}

				reader := bufio.NewReader(resp.Body)
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						if errors.Is(err, io.EOF) {
							close(respCh)
							return
						} else {
							errCh <- err
						}
					}

					var streamingResp *StreamingResponse
					if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &streamingResp); err == nil {
						respCh <- streamingResp.Choices[0].Delta.Content
					}
				}
			}()

			select {
			case err := <-errCh:
				log.Println("received error:", err)
				return nil
			default:
			}

			fmt.Fprintln(textView, "[green::]ChatGPT:[-]")
			go func() {
				var fullContent strings.Builder
				for deltaContent := range respCh {
					fmt.Fprintf(textView, deltaContent)
					fullContent.WriteString(deltaContent)
				}

				messages = append(messages, Message{
					Role:    roleAssistant,
					Content: fullContent.String(),
				})

				if list.GetItemCount() == 0 || isNewChat {
					list.InsertItem(0, strings.Trim(<-titleCh, "\""), "", rune(0), nil)
					list.SetCurrentItem(0)

					isNewChat = false
				}

				title, _ := list.GetItemText(list.GetCurrentItem())
				c := &Conversation{
					Time: time.Now().Unix(),
				}
				// no need to save the system message into db
				if messages[0].Role == roleSystem {
					c.Messages = messages[1:]
				} else {
					c.Messages = messages
				}

				value, err := json.Marshal(c)
				if err != nil {
					log.Panic(err)
				}
				db.Update(func(tx *buntdb.Tx) error {
					_, _, err := tx.Set(title, string(value), nil)
					return err
				})
				m[title] = c

				fmt.Fprintf(textView, "\n\n")
				textArea.SetDisabled(false)
			}()

			return nil
		}
		return event
	})

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if textView.GetText(false) != "" {
			list.SetSelectedFocusOnly(false)
		}

		switch event.Key() {
		case tcell.KeyF1:
			isNewChat = true
			list.SetSelectedFocusOnly(true)
			textView.Clear()
			app.SetFocus(textArea)
		case tcell.KeyF2:
			if list.GetItemCount() > 0 {
				app.SetFocus(list)
				title, _ := list.GetItemText(list.GetCurrentItem())
				textView.SetText(toConversation(m[title].Messages))
			}
		case tcell.KeyF3:
			if textView.GetText(false) != "" {
				app.SetFocus(textView)
			}
		case tcell.KeyF4:
			app.SetFocus(textArea)
		case tcell.KeyCtrlS:
			if list.GetItemCount() > 0 {
				app.SetFocus(searchInputField)
			}
		default:
			return event
		}
		return nil
	})

	help := tview.NewTextView().SetRegions(true).SetDynamicColors(true)
	help.SetText("F1: new chat, F2: history, F3: conversation, F4: question, enter: submit, ctrl-s: search, j/k: down/up, e: edit, d: delete, ctrl-f/b: page down/up, ctrl-c: quit").SetTextAlign(tview.AlignCenter)

	mainFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tview.NewFlex().SetDirection(tview.FlexColumn).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(searchInputField, 3, 1, false).
				AddItem(list, 0, 1, false), 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(textView, 0, 1, false).
				AddItem(textArea, 5, 1, false), 0, 3, false), 0, 1, false).
		AddItem(help, 1, 1, false)
	pages.
		AddPage(pageMain, mainFlex, true, true).
		AddPage(pageEditTitle, modal(editTitleInputField, list.GetCurrentItem()), true, false).
		AddPage(pageDeleteTitle, deleteTitleModal, true, false)
	if err := app.SetRoot(pages, true).SetFocus(textArea).Run(); err != nil {
		panic(err)
	}
}

func flock(f *os.File, timeout time.Duration) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return nil
			} else if err != syscall.EWOULDBLOCK {
				return err
			}
		case <-timer.C:
			return errTimeout
		}
	}
}

func NumTokensFromMessages(messages []Message, model string) (int, error) {
	t, err := tiktoken.EncodingForModel(model)
	if err != nil {
		return 0, err
	}

	var tokensPerMessage int
	if model == gpt3Dot5Turbo {
		tokensPerMessage = 4
	} else {
		tokensPerMessage = 3
	}

	numTokens := 0
	for _, message := range messages {
		numTokens += tokensPerMessage
		numTokens += len(t.Encode(message.Content, nil, nil))
		numTokens += len(t.Encode(message.Role, nil, nil))
	}
	numTokens += 3
	return numTokens, nil
}

func addSuffixNumber(title string) string {
	re := regexp.MustCompile(`(.*)\s-\s(\d+)$`)
	match := re.FindStringSubmatch(title)
	if match == nil {
		return fmt.Sprintf("%s - %d", title, 2)
	}
	suffixNumber, _ := strconv.Atoi(match[2])
	return fmt.Sprintf("%s - %d", match[1], suffixNumber+1)
}

const (
	completionsURL = "https://api.openai.com/v1/chat/completions"
	gpt3Dot5Turbo  = "gpt-3.5-turbo"
)

func createChatCompletion(messages []Message, stream bool) (*http.Response, error) {
	reqBody, err := json.Marshal(&Request{
		Model:    gpt3Dot5Turbo,
		Messages: messages,
		Stream:   stream,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, completionsURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	return client.Do(req)
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Response struct {
	Id      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type StreamingResponse struct {
	Id      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Index        int         `json:"index"`
		FinishReason interface{} `json:"finish_reason"`
	} `json:"choices"`
}

func toConversation(messages []Message) string {
	contents := make([]string, 0)
	for _, msg := range messages {
		switch msg.Role {
		case roleUser:
			msg.Content = fmt.Sprintf("[red::]You:[-]\n%s", msg.Content)
		case roleAssistant:
			msg.Content = fmt.Sprintf("[green::]ChatGPT:[-]\n%s", msg.Content)
		}
		contents = append(contents, msg.Content)
	}
	return strings.Join(contents, "\n\n")
}
