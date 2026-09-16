package sms

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/sslim7/nature-was/internal/messaging"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/recipients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Store interface {
	Create(context.Context, string, CreateRequest) (Campaign, bool, error)
	Get(context.Context, string, string) (document, error)
	List(context.Context, string, int, string) ([]Campaign, *string, error)
	Apply(context.Context, string, string, string, string, PatchRequest) (Result, error)
}
type FirestoreStore struct{ Client *firestore.Client }

func (s *FirestoreStore) collection(uid string) *firestore.CollectionRef {
	return s.Client.Collection("users").Doc(uid).Collection("smsCampaigns")
}
func mapError(err error) error {
	if status.Code(err) == codes.NotFound {
		return ErrNotFound
	}
	return err
}
func (s *FirestoreStore) Create(ctx context.Context, uid string, req CreateRequest) (Campaign, bool, error) {
	if err := req.validate(); err != nil {
		return Campaign{}, false, err
	}
	b, _ := json.Marshal(req)
	hash := sha256.Sum256(b)
	fingerprint := hex.EncodeToString(hash[:])
	key := sha256.Sum256([]byte(req.RequestID))
	ref := s.collection(uid).Doc(hex.EncodeToString(key[:]))
	var out Campaign
	var created bool
	err := s.Client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		// 경합 시 콜백이 재실행되므로 이전 시도의 생성 결과를 유지하지 않는다.
		created = false
		out = Campaign{}
		snap, err := tx.Get(ref)
		if err == nil {
			var d document
			if err = snap.DataTo(&d); err != nil {
				return err
			}
			if d.Fingerprint != fingerprint {
				return ErrIdempotency
			}
			out = d.Campaign
			return nil
		}
		if status.Code(err) != codes.NotFound {
			return err
		}
		attachments, e := messaging.Resolve(tx, s.Client, uid, req.AttachmentIDs)
		if e != nil {
			if errors.Is(e, messaging.ErrInvalid) {
				return ErrValidation
			}
			if errors.Is(e, messaging.ErrMissing) {
				return ErrNotFound
			}
			return e
		}
		now := time.Now().UTC()
		d := document{Fingerprint: fingerprint, Campaign: Campaign{Attachments: attachments, ID: ref.ID, Title: req.Title, Message: req.Message, Status: Ready, RecipientCount: len(req.RecipientIDs), ReadyCount: len(req.RecipientIDs), CreatedAt: now, UpdatedAt: now}, Recipients: make([]CampaignRecipient, 0, len(req.RecipientIDs))}
		phones := map[string]bool{}
		for _, id := range req.RecipientIDs {
			snap, err := tx.Get(recipients.DocumentRef(s.Client, uid, id))
			if err != nil {
				return mapError(err)
			}
			var r recipients.Recipient
			if err = snap.DataTo(&r); err != nil {
				return err
			}
			r.Phone = recipients.StoredPhone(r.Phone)
			if phones[r.Phone] {
				return ErrValidation
			}
			phones[r.Phone] = true
			d.Recipients = append(d.Recipients, CampaignRecipient{Attachments: attachments, ID: s.collection(uid).NewDoc().ID, CampaignID: ref.ID, RecipientID: id, Name: r.Name, Phone: r.Phone, Message: req.Message, Status: Ready, CreatedAt: now, UpdatedAt: now, Attempts: []Attempt{}})
		}
		if err := tx.Create(ref, d); err != nil {
			return err
		}
		out = d.Campaign
		created = true
		return nil
	})
	return out, created, err
}
func (s *FirestoreStore) Get(ctx context.Context, uid, id string) (document, error) {
	if !validID(id) {
		return document{}, ErrValidation
	}
	snap, err := s.collection(uid).Doc(id).Get(ctx)
	if err != nil {
		return document{}, mapError(err)
	}
	var d document
	err = snap.DataTo(&d)
	return d, err
}
func (s *FirestoreStore) Apply(ctx context.Context, uid, id, rid, action string, req PatchRequest) (Result, error) {
	if !validID(id) || (rid != "" && !validID(rid)) {
		return Result{}, ErrValidation
	}
	ref := s.collection(uid).Doc(id)
	baseline := 0
	if action == "patch" && req.Status == Sent {
		current, e := s.Get(ctx, uid, id)
		if e != nil {
			return Result{}, e
		}
		originalID := ""
		for _, r := range current.Recipients {
			if r.ID == rid {
				originalID = r.RecipientID
				break
			}
		}
		if originalID != "" {
			baseline, e = (&recipients.Store{FS: s.Client}).SuccessfulCount(ctx, uid, originalID)
			if e != nil {
				return Result{}, e
			}
		}
	}
	var out Result
	err := s.Client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		out = Result{}
		snap, err := tx.Get(ref)
		if err != nil {
			return mapError(err)
		}
		var d document
		if err = snap.DataTo(&d); err != nil {
			return err
		}
		previousRevision := d.Revision
		result, err := d.apply(rid, action, req, time.Now().UTC())
		if err != nil {
			return err
		}
		// 멱등 재요청은 쓰기 잠금을 늘리지 않고 기존 결과만 돌려준다.
		if d.Revision == previousRevision {
			out = result
			return nil
		}
		var recipientDoc *firestore.DocumentSnapshot
		if action == "patch" && req.Status == Sent {
			recipientDoc, err = tx.Get(recipients.DocumentRef(s.Client, uid, result.Recipient.RecipientID))
			if err != nil && status.Code(err) != codes.NotFound {
				return err
			}
		}
		if action == "patch" && (req.Status == Sent || req.Status == Failed) {
			history := recipients.History{Source: "ANDROID", Transport: result.Recipient.Transport, ID: id + "_" + rid + "_" + req.AttemptID, CampaignTitle: result.Campaign.Title, CampaignID: id, RecipientID: result.Recipient.RecipientID, Name: result.Recipient.Name, Phone: result.Recipient.Phone, Message: result.Recipient.Message, Status: result.Recipient.Status, SentAt: result.Recipient.SentAt, FailedAt: result.Recipient.FailedAt, ErrorCode: result.Recipient.ErrorCode, ErrorMessage: result.Recipient.ErrorMessage, CreatedAt: result.Recipient.CreatedAt, UpdatedAt: result.Recipient.UpdatedAt, Attachments: result.Recipient.Attachments}
			if err = tx.Set(recipients.DocumentRef(s.Client, uid, result.Recipient.RecipientID).Collection("history").Doc(history.ID), history); err != nil {
				return err
			}
			if recipientDoc != nil && recipientDoc.Exists() && req.Status == Sent {
				var current recipients.Recipient
				if err = recipientDoc.DataTo(&current); err != nil {
					return err
				}
				count := current.SentCount
				if baseline > count {
					count = baseline
				}
				updates := []firestore.Update{{Path: "sentCount", Value: count + 1}}
				if current.LatestSentAt == nil || current.LatestSentAt.Before(*result.Recipient.SentAt) {
					updates = append(updates, firestore.Update{Path: "latestSentAt", Value: result.Recipient.SentAt})
				}
				if err = tx.Update(recipientDoc.Ref, updates); err != nil {
					return err
				}
			}
			// 상세 오류 이력은 별도 문서에 보관해 캠페인 문서의 1MiB 제한을 지킨다.
			key := sha256.Sum256([]byte(rid + ":" + req.AttemptID))
			if err = tx.Set(ref.Collection("attempts").Doc(hex.EncodeToString(key[:])), result.Recipient); err != nil {
				return err
			}
		}
		if err = tx.Set(ref, d); err != nil {
			return err
		}
		out = result
		return nil
	})
	return out, err
}

type listCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
	UserID    string    `json:"u"`
}

func (s *FirestoreStore) List(ctx context.Context, uid string, limit int, cursor string) ([]Campaign, *string, error) {
	if limit < 1 || limit > 100 {
		return nil, nil, ErrValidation
	}
	q := s.collection(uid).OrderBy("campaign.createdAt", firestore.Desc).OrderBy(firestore.DocumentID, firestore.Desc)
	if cursor != "" {
		var c listCursor
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(b, &c) != nil || c.CreatedAt.IsZero() || !validID(c.ID) || c.UserID != uid {
			return nil, nil, ErrValidation
		}
		q = q.StartAfter(c.CreatedAt, s.collection(uid).Doc(c.ID))
	}
	snaps, err := q.Limit(limit + 1).Documents(ctx).GetAll()
	if err != nil {
		return nil, nil, err
	}
	items := make([]Campaign, 0, limit)
	for i, snap := range snaps {
		if i == limit {
			break
		}
		var d document
		if err = snap.DataTo(&d); err != nil {
			return nil, nil, err
		}
		items = append(items, d.Campaign)
	}
	var next *string
	if len(snaps) > limit {
		last := items[len(items)-1]
		b, _ := json.Marshal(listCursor{last.CreatedAt, last.ID, uid})
		c := base64.RawURLEncoding.EncodeToString(b)
		next = &c
	}
	return items, next, nil
}
