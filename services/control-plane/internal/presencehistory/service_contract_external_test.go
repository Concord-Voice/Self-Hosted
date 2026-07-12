package presencehistory_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presencehistory"
	"github.com/stretchr/testify/require"
)

type plannedCustomTextRecorder interface {
	RecordCustomTextTransition(
		context.Context,
		*sql.Tx,
		uuid.UUID,
		presencehistory.CustomTextState,
		presencehistory.CustomTextState,
	) error
}

var _ plannedCustomTextRecorder = (*presencehistory.Service)(nil)

func TestServiceRecorderFailsClosedWithoutConstruction(t *testing.T) {
	var service *presencehistory.Service
	err := service.RecordCustomTextTransition(
		context.Background(),
		nil,
		uuid.New(),
		presencehistory.CustomTextState{},
		presencehistory.CustomTextState{Text: "not recorded"},
	)
	require.Error(t, err)

	service = presencehistory.NewService(nil, presencehistory.DisclosureState{}, false)
	err = service.RecordCustomTextTransition(
		context.Background(),
		nil,
		uuid.New(),
		presencehistory.CustomTextState{},
		presencehistory.CustomTextState{Text: "not recorded"},
	)
	require.Error(t, err)
}
