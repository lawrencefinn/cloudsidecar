package object

import (
	"cloud.google.com/go/storage"
	"encoding/xml"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/s3manager"
	uuid2 "github.com/google/uuid"
	"github.com/gorilla/mux"
	"io"
	"net/http"
	"os"
	s3_handler "sidecar/pkg/aws/handler/s3"
	"sidecar/pkg/converter"
	"sidecar/pkg/logging"
	"strconv"
	"strings"
	"time"
)

type Handler struct {
	*s3_handler.Handler
}

type Bucket interface {
	HeadHandle(writer http.ResponseWriter, request *http.Request)
	HeadParseInput(r *http.Request) (*s3.HeadObjectInput, error)
	GetHandle(writer http.ResponseWriter, request *http.Request)
	GetParseInput(r *http.Request) (*s3.GetObjectInput, error)
	PutHandle(writer http.ResponseWriter, request *http.Request)
	PutParseInput(r *http.Request) (*s3manager.UploadInput, error)
	MultiPartHandle(writer http.ResponseWriter, request *http.Request)
	MultiPartParseInput(r *http.Request) (*s3.CreateMultipartUploadInput, error)
	UploadPartHandle(writer http.ResponseWriter, request *http.Request)
	UploadPartParseInput(r *http.Request) (*s3.UploadPartInput, error)
	New(s3Handler *s3_handler.Handler) Handler
}

func (handler *Handler) UploadPartHandle(writer http.ResponseWriter, request *http.Request) {
	s3Req, _ := handler.UploadPartParseInput(request)
	var resp *s3.UploadPartOutput
	var err error
	if handler.GCPClient != nil {
		key := fmt.Sprintf("%s-part-%d", *s3Req.Key, *s3Req.PartNumber)
		uploader := handler.GCPClient.Bucket(*s3Req.Bucket).Object(key).NewWriter(*handler.Context)
		gReq, _ := handler.PutParseInput(request)
		_, err := converter.GCPUpload(gReq, uploader)
		uploader.Close()
		if err != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s\n", err)
			writer.Write([]byte(string(fmt.Sprint(err))))
			return
		}
		attrs, _ := handler.GCPClient.Bucket(*s3Req.Bucket).Object(key).Attrs(*handler.Context)
		converter.GCSAttrToHeaders(attrs, writer)
		path := fmt.Sprintf("%s/%s", handler.Config.GetString("gcp_destination_config.gcs_config.multipart_db_directory"), *s3Req.UploadId)
		logging.Log.Info(path)
		f, fileErr := os.Create(path)
		if fileErr != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", fileErr)
			writer.Write([]byte(string(fmt.Sprint(fileErr))))
			return
		}
		defer f.Close()
		_, fileErr = f.WriteString(fmt.Sprintf("%s,%s\n", writer.Header().Get("ETag"), key))
		if fileErr != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", fileErr)
			writer.Write([]byte(string(fmt.Sprint(fileErr))))
			return
		}
	} else {
		req := handler.S3Client.UploadPartRequest(s3Req)
		req.HTTPRequest.Header.Set("Content-Length", request.Header.Get("Content-Length"))
		req.HTTPRequest.Header.Set("X-Amz-Content-Sha256", request.Header.Get("X-Amz-Content-Sha256"))
		req.Body = aws.ReadSeekCloser(request.Body)
		resp, err = req.Send()
		if err != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error in uploading %s", err)
			writer.Write([]byte(string(fmt.Sprint(err))))
			return
		}
		logging.Log.Info(fmt.Sprint(resp))
		if header := resp.ETag; header != nil {
			writer.Header().Set("ETag", *header)
		}
	}
	writer.WriteHeader(200)
	writer.Write([]byte(""))
}
func (handler *Handler) UploadPartParseInput(r *http.Request) (*s3.UploadPartInput, error) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	key := vars["key"]
	partNumber, err := strconv.ParseInt(vars["partNumber"], 10, 64)
	uploadId := vars["uploadId"]
	return &s3.UploadPartInput{
		Bucket: &bucket,
		Key: &key,
		PartNumber: &partNumber,
		UploadId: &uploadId,
	}, err
}

func (handler *Handler) MultiPartHandle(writer http.ResponseWriter, request *http.Request){
	s3Req, _ := handler.MultiPartParseInput(request)
	var resp *s3.CreateMultipartUploadOutput
	var err error
	if handler.GCPClient != nil {
		uuid := uuid2.New().String()
		path := fmt.Sprintf("%s/%s", handler.Config.GetString("gcp_destination_config.gcs_config.multipart_db_directory"), uuid)
		f, fileErr := os.Create(path)
		if fileErr != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", err)
			writer.Write([]byte(string(fmt.Sprint(err))))
			return
		}
		defer f.Close()
		logging.Log.Info(uuid)
		resp = &s3.CreateMultipartUploadOutput{
			Key: s3Req.Key,
			Bucket: s3Req.Bucket,
			UploadId: &uuid,
		}
	} else {
		req := handler.S3Client.CreateMultipartUploadRequest(s3Req)
		resp, err = req.Send()
	}
	if err != nil {
		writer.WriteHeader(404)
		logging.Log.Error("Error %s", err)
		writer.Write([]byte(string(fmt.Sprint(err))))
		return
	}
	output, _ := xml.MarshalIndent(resp, "  ", "    ")
	writer.Write([]byte(s3_handler.XmlHeader))
	writer.Write([]byte(string(output)))
}

func (handler *Handler) MultiPartParseInput(r *http.Request) (*s3.CreateMultipartUploadInput, error) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	key := vars["key"]
	s3Req := &s3.CreateMultipartUploadInput{
		Bucket: &bucket,
		Key:    &key,
	}
	return s3Req, nil
}

func (handler *Handler) PutParseInput(r *http.Request) (*s3manager.UploadInput, error) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	key := vars["key"]
	s3Req := &s3manager.UploadInput{
		Bucket: &bucket,
		Key: &key,
	}
	if header := r.Header.Get("Content-MD5"); header != "" {
		s3Req.ContentMD5 = &header
	}
	if header := r.Header.Get("Content-Type"); header != "" {
		s3Req.ContentType = &header
	}
	isChunked := false
	if header := r.Header.Get("x-amz-content-sha256"); header == "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" {
		isChunked = true
	}
	var contentLength int64
	if header := r.Header.Get("x-amz-decoded-content-length"); header != "" {
		contentLength, _ = strconv.ParseInt(header, 10, 64)
	} else if header := r.Header.Get("x-amz-decoded-content-length"); header != "" {
		contentLength, _ = strconv.ParseInt(header, 10, 64)
	}
	if !isChunked {
		s3Req.Body = r.Body
	} else {
		logging.Log.Debug("CHUNKED %d", contentLength)
		readerWrapper := ChunkedReaderWrapper{
			Reader:         &r.Body,
			ChunkNextPosition: -1,
		}
		s3Req.Body = &readerWrapper
	}
	return s3Req, nil
}

func (handler *Handler) PutHandle(writer http.ResponseWriter, request *http.Request){
	s3Req, _ := handler.PutParseInput(request)
	var err error
	// wg := sync.WaitGroup{}
	defer request.Body.Close()
	if handler.GCPClient != nil {
		bucket := handler.BucketRename(*s3Req.Bucket)
		uploader := handler.GCPClient.Bucket(bucket).Object(*s3Req.Key).NewWriter(*handler.Context)
		defer uploader.Close()
		_, err := converter.GCPUpload(s3Req, uploader)
		if err != nil {
			logging.Log.Error("Error %s\n", err)
		}
	} else {
		uploader := s3manager.NewUploaderWithClient(handler.S3Client)
		_, err = uploader.Upload(s3Req)
		if err != nil {
			logging.Log.Error("Error %s", err)
		}
	}
	writer.WriteHeader(200)
	writer.Write([]byte(""))
}


func (handler *Handler) GetParseInput(r *http.Request) (*s3.GetObjectInput, error) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	key := vars["key"]
	return &s3.GetObjectInput{Bucket: &bucket, Key: &key}, nil
}

func (handler *Handler) GetHandle(writer http.ResponseWriter, request *http.Request) {
	input, _ := handler.GetParseInput(request)
	if header := request.Header.Get("Range"); header != "" {
		input.Range = &header
	}
	if handler.GCPClient != nil {
		bucket := handler.BucketRename(*input.Bucket)
		objHandle := handler.GCPClient.Bucket(bucket).Object(*input.Key)
		attrs, err := objHandle.Attrs(*handler.Context)
		if err != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", err)
			return
		}
		converter.GCSAttrToHeaders(attrs, writer)
		var reader *storage.Reader
		var readerError error
		if input.Range != nil {
			equalSplit := strings.SplitN(*input.Range, "=", 2)
			byteSplit := strings.SplitN(equalSplit[1], "-", 2)
			startByte, _ := strconv.ParseInt(byteSplit[0], 10, 64)
			length := int64(-1)
			if len(byteSplit) > 1 {
				endByte, _ := strconv.ParseInt(byteSplit[1], 10, 64)
				length = endByte + 1 - startByte
			}
			reader, readerError = objHandle.NewRangeReader(*handler.Context, startByte, length)
		} else {
			reader, readerError = objHandle.NewReader(*handler.Context)
		}
		if readerError != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", readerError)
			return
		}
		defer reader.Close()
		buffer := make([]byte, 4096)
		for {
			n, err := reader.Read(buffer)
			if n > 0 {
				writer.Write(buffer[:n])
			}
			if err == io.EOF {
				break
			}
		}
	} else {
		req := handler.S3Client.GetObjectRequest(input)
		resp, respError := req.Send()
		if respError != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", respError)
			return
		}
		if header := resp.ServerSideEncryption; header != "" {
			writer.Header().Set("ServerSideEncryption", string(header))
		}
		if header := resp.LastModified; header != nil {
			lastMod := header.Format(time.RFC1123)
			lastMod = strings.Replace(lastMod, "UTC", "GMT", 1)
			writer.Header().Set("Last-Modified", lastMod)
		}
		if header := resp.ContentRange; header != nil {
			writer.Header().Set("ContentRange", *header)
		}
		if header := resp.ETag; header != nil {
			writer.Header().Set("ETag", *header)
		}
		if header := resp.ContentLength; header != nil {
			writer.Header().Set("Content-Length", strconv.FormatInt(*header, 10))
		}
		defer resp.Body.Close()
		buffer := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buffer)
			if n > 0 {
				writer.Write(buffer[:n])
			}
			if err == io.EOF {
				break
			}
		}
	}
	return
}

func (handler *Handler) HeadParseInput(r *http.Request) (*s3.HeadObjectInput, error) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	key := vars["key"]
	return &s3.HeadObjectInput{Bucket: &bucket, Key: &key}, nil
}

func (handler *Handler) HeadHandle(writer http.ResponseWriter, request *http.Request) {
	input, _ := handler.HeadParseInput(request)
	if handler.GCPClient != nil {
		bucket := handler.BucketRename(*input.Bucket)
		resp, err := handler.GCPClient.Bucket(bucket).Object(*input.Key).Attrs(*handler.Context)
		if err != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", err)
			return
		}
		converter.GCSAttrToHeaders(resp, writer)
	} else {
		req := handler.S3Client.HeadObjectRequest(input)
		resp, respError := req.Send()
		if respError != nil {
			writer.WriteHeader(404)
			logging.Log.Error("Error %s", respError)
			return
		}
		if resp.AcceptRanges != nil {
			writer.Header().Set("Accept-Ranges", *resp.AcceptRanges)
		}
		if resp.ContentLength != nil {
			writer.Header().Set("Content-Length", strconv.FormatInt(*resp.ContentLength, 10))
		}
		if resp.ServerSideEncryption != "" {
			writer.Header().Set("x-amz-server-side-encryption", string(resp.ServerSideEncryption))
		}
		if resp.CacheControl != nil {
			writer.Header().Set("Cache-Control", *resp.CacheControl)
		}
		if resp.ContentType != nil {
			writer.Header().Set("Content-Type", *resp.ContentType)
		}
		if resp.ETag != nil {
			writer.Header().Set("ETag", *resp.ETag)
		}
		if resp.LastModified != nil {
			lastMod := resp.LastModified.Format(time.RFC1123)
			lastMod = strings.Replace(lastMod, "UTC", "GMT", 1)
			writer.Header().Set("Last-Modified", lastMod)
		}
	}
	writer.WriteHeader(200)
	return
}

func New(s3Handler *s3_handler.Handler) *Handler {
	return &Handler{s3Handler}
}