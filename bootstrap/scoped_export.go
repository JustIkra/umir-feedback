package bootstrap

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"syscall"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func scopedExport(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var gateway, restaurant, sessionFile, cursor string
	limit := 100
	flags.StringVar(&gateway, "gateway-url", "", "HTTPS Gateway origin")
	flags.StringVar(&restaurant, "restaurant-id", "", "explicit restaurant UUID")
	flags.StringVar(&sessionFile, "session-file", "", "private Admin session token file")
	flags.StringVar(&cursor, "cursor", "", "scope cursor")
	flags.IntVar(&limit, "limit", 100, "page limit")
	if flags.Parse(args) != nil || flags.NArg() != 0 || restaurant != strings.ToLower(restaurant) || !domain.ValidID(domain.SubmissionID(restaurant)) || sessionFile == "" || limit < 1 || limit > 500 {
		return errors.New("export requires Gateway URL, explicit restaurant and Admin session file")
	}
	u, e := url.Parse(gateway)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("invalid --gateway-url")
	}
	file, e := os.OpenFile(sessionFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return errors.New("session file unavailable")
	}
	info, se := file.Stat()
	if se != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		ce := file.Close()
		return errors.Join(errors.New("unsafe session file"), ce)
	}
	token, re := io.ReadAll(io.LimitReader(file, 4097))
	ce := file.Close()
	defer clear(token)
	if re != nil || ce != nil || len(token) == 0 || len(token) > 4096 || strings.ContainsAny(string(token), "\r\n; \t\x00") {
		return errors.New("invalid session file")
	}
	u.Path = "/api/v2/r/" + restaurant + "/feedback/export"
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u.RawQuery = q.Encode()
	r, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return errors.New("export request unavailable")
	}
	r.AddCookie(&http.Cookie{Name: "__Host-umir_operator_session", Value: string(token)})
	r.Header.Set("X-Umir-Activity", "background")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, e := client.Do(r)
	if e != nil {
		return errors.New("gateway export unavailable")
	}
	body, re := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	ce = response.Body.Close()
	if re != nil || ce != nil || len(body) > 1<<20 || response.StatusCode != 200 {
		return errors.New("gateway export not authorized or unavailable")
	}
	if err := validateExport(body, restaurant, limit); err != nil {
		return err
	}
	if _, e := out.Write(body); e != nil {
		return errors.New("export output failed")
	}
	return nil
}
