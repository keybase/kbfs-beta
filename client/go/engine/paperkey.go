// PaperKey creates paper backup keys for a user and pushes them to the server.
// It checks for existing paper devices and offers to revoke the
// keys.
//

package engine

import (
	"fmt"

	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/protocol/go"
)

// PaperKey is an engine.
type PaperKey struct {
	passphrase libkb.PaperKeyPhrase
	libkb.Contextified
}

// NewPaperKey creates a PaperKey engine.
func NewPaperKey(g *libkb.GlobalContext) *PaperKey {
	return &PaperKey{
		Contextified: libkb.NewContextified(g),
	}
}

// Name is the unique engine name.
func (e *PaperKey) Name() string {
	return "PaperKey"
}

// GetPrereqs returns the engine prereqs.
func (e *PaperKey) Prereqs() Prereqs {
	return Prereqs{
		Device: true,
	}
}

// RequiredUIs returns the required UIs.
func (e *PaperKey) RequiredUIs() []libkb.UIKind {
	return []libkb.UIKind{
		libkb.LoginUIKind,
	}
}

// SubConsumers returns the other UI consumers for this engine.
func (e *PaperKey) SubConsumers() []libkb.UIConsumer {
	return []libkb.UIConsumer{
		&RevokeEngine{},
		&PaperKeyGen{},
	}
}

// Run starts the engine.
func (e *PaperKey) Run(ctx *Context) error {
	me, err := libkb.LoadMe(libkb.NewLoadUserArg(e.G()))
	if err != nil {
		return err
	}

	// check for existing paper keys
	cki := me.GetComputedKeyInfos()
	if cki == nil {
		return fmt.Errorf("no computed key infos")
	}
	var needReload bool
	for i, bdev := range cki.PaperDevices() {
		revoke, err := ctx.LoginUI.PromptRevokePaperKeys(
			keybase1.PromptRevokePaperKeysArg{
				Device: *bdev.ProtExport(),
				Index:  i,
			})
		if err != nil {
			e.G().Log.Warning("prompt error: %s", err)
			continue
		}
		if !revoke {
			continue
		}
		reng := NewRevokeDeviceEngine(RevokeDeviceEngineArgs{ID: bdev.ID}, e.G())
		if err := RunEngine(reng, ctx); err != nil {
			// probably not a good idea to continue...
			return err
		}
		needReload = true
	}

	if needReload {
		me, err = libkb.LoadMe(libkb.NewLoadUserArg(e.G()))
		if err != nil {
			return err
		}
	}

	signingKey, _, err := e.G().Keyrings.GetSecretKeyWithPrompt(ctx.LoginContext, libkb.SecretKeyArg{
		Me:      me,
		KeyType: libkb.DeviceSigningKeyType,
	}, ctx.SecretUI, "You must sign your new paper key")
	if err != nil {
		return err
	}

	e.passphrase, err = libkb.MakePaperKeyPhrase(libkb.PaperKeyVersion)
	if err != nil {
		return err
	}

	kgarg := &PaperKeyGenArg{
		Passphrase: e.passphrase,
		Me:         me,
		SigningKey: signingKey,
	}
	kgeng := NewPaperKeyGen(kgarg, e.G())
	if err := RunEngine(kgeng, ctx); err != nil {
		return err
	}

	return ctx.LoginUI.DisplayPaperKeyPhrase(keybase1.DisplayPaperKeyPhraseArg{Phrase: e.passphrase.String()})

}

func (e *PaperKey) Passphrase() string {
	return e.passphrase.String()
}