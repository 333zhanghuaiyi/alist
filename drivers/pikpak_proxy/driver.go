package PikPakProxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alist-org/alist/v3/internal/op"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	hash_extend "github.com/alist-org/alist/v3/pkg/utils/hash"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type PikPakProxy struct {
	model.Storage
	Addition
	*Common
	RefreshToken string
	AccessToken  string
}

func (d *PikPakProxy) Config() driver.Config {
	return config
}

func (d *PikPakProxy) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *PikPakProxy) Init(ctx context.Context) (err error) {
	if d.ClientID == "" || d.ClientSecret == "" {
		d.ClientID = "YNxT9w7GMdWvEOKa"
		d.ClientSecret = "dbw2OtmVEeuUvIptb1Coyg"
	}

	if d.Common == nil {
		d.Common = &Common{
			client:       base.NewRestyClient(),
			CaptchaToken: "",
			UserID:       "",
			UseProxy:     d.Addition.UseProxy,
			ProxyUrl:     d.Addition.ProxyUrl,
			DeviceID:     utils.GetMD5EncodeStr(d.Username + d.Password),
			UserAgent:    BuildCustomUserAgent(utils.GetMD5EncodeStr(d.Username+d.Password), ClientID, PackageName, SdkVersion, ClientVersion, PackageName, ""),
			RefreshCTokenCk: func(token string) {
				d.Common.CaptchaToken = token
				op.MustSaveDriverStorage(d)
			},
		}
	}

	if d.Addition.CaptchaToken != "" && d.Addition.RefreshToken == "" {
		d.SetCaptchaToken(d.Addition.CaptchaToken)
	}

	// 如果已经有RefreshToken，直接刷新AccessToken
	if d.Addition.RefreshToken != "" {
		d.RefreshToken = d.Addition.RefreshToken
		if err := d.refreshToken(); err != nil {
			return err
		}
	} else {
		if err := d.login(); err != nil {
			return err
		}
	}

	// 获取CaptchaToken
	err = d.RefreshCaptchaTokenAtLogin(GetAction(http.MethodGet, "https://api-drive.mypikpak.com/drive/v1/files"), d.Common.UserID)
	if err != nil {
		return err
	}
	// 更新UserAgent
	d.Common.UserAgent = BuildCustomUserAgent(d.Common.DeviceID, ClientID, PackageName, SdkVersion, ClientVersion, PackageName, d.Common.UserID)
	return nil
}

func (d *PikPakProxy) Drop(ctx context.Context) error {
	return nil
}

func (d *PikPakProxy) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *PikPakProxy) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	var resp File
	_, err := d.requestWithCaptchaToken(fmt.Sprintf("https://api-drive.mypikpak.com/drive/v1/files/%s?_magic=2021&thumbnail_size=SIZE_LARGE", file.GetID()),
		http.MethodGet, nil, &resp)
	if err != nil {
		return nil, err
	}
	// _, err := d.request(fmt.Sprintf("https://api-drive.mypikpak.com/drive/v1/files/%s?_magic=2021&thumbnail_size=SIZE_LARGE", file.GetID()),
	// 	http.MethodGet, nil, &resp)
	// if err != nil {
	// 	return nil, err
	// }
	link := model.Link{
		URL: resp.WebContentLink,
	}
	if !d.DisableMediaLink && len(resp.Medias) > 0 && resp.Medias[0].Link.Url != "" {
		log.Debugln("use media link")
		link.URL = resp.Medias[0].Link.Url
	}

	if d.Addition.UseProxy {
		if strings.HasSuffix(d.Addition.ProxyUrl, "/") {
			link.URL = d.Addition.ProxyUrl + link.URL
		} else {
			link.URL = d.Addition.ProxyUrl + "/" + link.URL
		}

	}

	return &link, nil
}

func (d *PikPakProxy) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"kind":      "drive#folder",
			"parent_id": parentDir.GetID(),
			"name":      dirName,
		})
	}, nil)
	return err
}

func (d *PikPakProxy) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchMove", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{srcObj.GetID()},
			"to": base.Json{
				"parent_id": dstDir.GetID(),
			},
		})
	}, nil)
	return err
}

func (d *PikPakProxy) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files/"+srcObj.GetID(), http.MethodPatch, func(req *resty.Request) {
		req.SetBody(base.Json{
			"name": newName,
		})
	}, nil)
	return err
}

func (d *PikPakProxy) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchCopy", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{srcObj.GetID()},
			"to": base.Json{
				"parent_id": dstDir.GetID(),
			},
		})
	}, nil)
	return err
}

func (d *PikPakProxy) Remove(ctx context.Context, obj model.Obj) error {
	//https://api-drive.mypikpak.com/drive/v1/files:batchTrash
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchDelete", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{obj.GetID()},
		})
	}, nil)
	return err
}

func (d *PikPakProxy) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	hi := stream.GetHash()
	sha1Str := hi.GetHash(hash_extend.GCID)
	if len(sha1Str) < hash_extend.GCID.Width {
		tFile, err := stream.CacheFullInTempFile()
		if err != nil {
			return err
		}

		sha1Str, err = utils.HashFile(hash_extend.GCID, tFile, stream.GetSize())
		if err != nil {
			return err
		}
	}

	var resp UploadTaskData
	res, err := d.request("https://api-drive.mypikpak.com/drive/v1/files", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"kind":        "drive#file",
			"name":        stream.GetName(),
			"size":        stream.GetSize(),
			"hash":        strings.ToUpper(sha1Str),
			"upload_type": "UPLOAD_TYPE_RESUMABLE",
			"objProvider": base.Json{"provider": "UPLOAD_TYPE_UNKNOWN"},
			"parent_id":   dstDir.GetID(),
			"folder_type": "NORMAL",
		})
	}, &resp)
	if err != nil {
		return err
	}

	// 秒传成功
	if resp.Resumable == nil {
		log.Debugln(string(res))
		return nil
	}

	params := resp.Resumable.Params
	endpoint := strings.Join(strings.Split(params.Endpoint, ".")[1:], ".")
	cfg := &aws.Config{
		Credentials: credentials.NewStaticCredentials(params.AccessKeyID, params.AccessKeySecret, params.SecurityToken),
		Region:      aws.String("pikpak"),
		Endpoint:    &endpoint,
	}
	ss, err := session.NewSession(cfg)
	if err != nil {
		return err
	}
	uploader := s3manager.NewUploader(ss)
	if stream.GetSize() > s3manager.MaxUploadParts*s3manager.DefaultUploadPartSize {
		uploader.PartSize = stream.GetSize() / (s3manager.MaxUploadParts - 1)
	}
	input := &s3manager.UploadInput{
		Bucket: &params.Bucket,
		Key:    &params.Key,
		Body:   stream,
	}
	_, err = uploader.UploadWithContext(ctx, input)
	return err
}

func (d *PikPakProxy) Offline(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	_, err := d.requestWithCaptchaToken("https://api-drive.mypikpak.com/drive/v1/files",
		http.MethodPost, func(r *resty.Request) {
			r.SetContext(ctx)
			r.SetBody(&base.Json{
				"kind":        "drive#file",
				"name":        "",
				"upload_type": "UPLOAD_TYPE_URL",
				"url": &base.Json{
					"url": args.Data,
				},
				"folder_type": "DOWNLOAD",
			})
		}, nil)
	if err != nil {
		return nil, err
	}
	return "ok", nil
}

/*
获取离线下载任务列表
phase 可能的取值：
PHASE_TYPE_RUNNING, PHASE_TYPE_ERROR, PHASE_TYPE_COMPLETE, PHASE_TYPE_PENDING
*/
func (d *PikPakProxy) OfflineList(ctx context.Context, nextPageToken string, phase []string) ([]OfflineTask, error) {
	res := make([]OfflineTask, 0)
	url := "https://api-drive.mypikpak.com/drive/v1/tasks"

	if len(phase) == 0 {
		phase = []string{"PHASE_TYPE_RUNNING", "PHASE_TYPE_ERROR", "PHASE_TYPE_COMPLETE", "PHASE_TYPE_PENDING"}
	}
	params := map[string]string{
		"type":           "offline",
		"thumbnail_size": "SIZE_SMALL",
		"limit":          "10000",
		"page_token":     nextPageToken,
		"with":           "reference_resource",
	}

	// 处理 phase 参数
	if len(phase) > 0 {
		filters := base.Json{
			"phase": map[string]string{
				"in": strings.Join(phase, ","),
			},
		}
		filtersJSON, err := json.Marshal(filters)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal filters: %w", err)
		}
		params["filters"] = string(filtersJSON)
	}

	var resp OfflineListResp
	_, err := d.request(url, http.MethodGet, func(req *resty.Request) {
		req.SetContext(ctx).
			SetQueryParams(params)
	}, &resp)

	if err != nil {
		return nil, fmt.Errorf("failed to get offline list: %w", err)
	}
	res = append(res, resp.Tasks...)
	return res, nil
}

func (d *PikPakProxy) DeleteOfflineTasks(ctx context.Context, taskIDs []string, deleteFiles bool) error {
	url := "https://api-drive.mypikpak.com/drive/v1/tasks"
	params := map[string]string{
		"task_ids":     strings.Join(taskIDs, ","),
		"delete_files": strconv.FormatBool(deleteFiles),
	}
	_, err := d.request(url, http.MethodDelete, func(req *resty.Request) {
		req.SetContext(ctx).
			SetQueryParams(params)
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to delete tasks %v: %w", taskIDs, err)
	}
	return nil
}

var _ driver.Driver = (*PikPakProxy)(nil)
