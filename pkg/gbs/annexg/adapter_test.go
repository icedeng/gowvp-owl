package annexg

import (
	"context"
	"errors"
	"testing"
)

type authorizerFunc func(context.Context, Exchange) error

func (fn authorizerFunc) AuthorizeAnnexG(ctx context.Context, exchange Exchange) error {
	return fn(ctx, exchange)
}

type consumerFunc func(context.Context, Exchange) (Message, error)

func (fn consumerFunc) ConsumeAnnexG(ctx context.Context, exchange Exchange) (Message, error) {
	return fn(ctx, exchange)
}

func testExchange(source SystemRole, destination SystemRole) Exchange {
	return Exchange{
		SourceID: "source", SourceRole: source,
		DestinationID: "destination", DestinationRole: destination,
	}
}

func TestAdapterDisabledByDefault(t *testing.T) {
	_, err := (Adapter{}).Handle(context.Background(), Version2011, Exchange{}, nil)
	if !errors.Is(err, ErrAdapterDisabled) {
		t.Fatalf("Handle() error = %v, want ErrAdapterDisabled", err)
	}
}

func TestAdapterValidatesBeforeConsumption(t *testing.T) {
	authorized := 0
	consumed := 0
	adapter := Adapter{
		Authorizer: authorizerFunc(func(context.Context, Exchange) error {
			authorized++
			return nil
		}),
		Consumer: consumerFunc(func(context.Context, Exchange) (Message, error) {
			consumed++
			return nil, nil
		}),
	}
	body := []byte(`<Notify><CmdType>TGSAlarm</CmdType><SN>1</SN><AlarmContent><AlarmTime>2026-08-27T10:20:30</AlarmTime><TollgateID>gate</TollgateID><CarPlate>plate</CarPlate><PlateType>02</PlateType><DefenceType>wanted</DefenceType></AlarmContent></Notify>`)

	t.Run("bad direction", func(t *testing.T) {
		_, err := adapter.Handle(context.Background(), Version2011, testExchange(RoleManagementPlatform, RoleTollgateSystem), body)
		if err == nil {
			t.Fatal("Handle() accepted an invalid direction")
		}
		if authorized != 0 || consumed != 0 {
			t.Fatalf("calls = authorized:%d consumed:%d, want zero", authorized, consumed)
		}
	})

	t.Run("bad XML", func(t *testing.T) {
		_, err := adapter.Handle(context.Background(), Version2011, testExchange(RoleTollgateSystem, RoleManagementPlatform), []byte(`<Notify/>`))
		if err == nil {
			t.Fatal("Handle() accepted invalid XML")
		}
		if authorized != 0 || consumed != 0 {
			t.Fatalf("calls = authorized:%d consumed:%d, want zero", authorized, consumed)
		}
	})
}

func TestAdapterAuthorizationAndCorrelation(t *testing.T) {
	body := []byte(`<Notify><CmdType>TGSAlarm</CmdType><SN>21</SN><AlarmContent><AlarmTime>2026-08-27T10:20:30</AlarmTime><TollgateID>gate</TollgateID><CarPlate>plate</CarPlate><PlateType>02</PlateType><DefenceType>wanted</DefenceType></AlarmContent></Notify>`)
	exchange := testExchange(RoleTollgateSystem, RoleManagementPlatform)

	t.Run("authorization denial", func(t *testing.T) {
		consumed := false
		adapter := Adapter{
			Authorizer: authorizerFunc(func(context.Context, Exchange) error { return errors.New("denied") }),
			Consumer: consumerFunc(func(context.Context, Exchange) (Message, error) {
				consumed = true
				return nil, nil
			}),
		}
		if _, err := adapter.Handle(context.Background(), Version2011, exchange, body); err == nil {
			t.Fatal("Handle() accepted an authorization denial")
		}
		if consumed {
			t.Fatal("consumer ran after authorization denial")
		}
	})

	t.Run("matching response", func(t *testing.T) {
		adapter := Adapter{
			Authorizer: authorizerFunc(func(context.Context, Exchange) error { return nil }),
			Consumer: consumerFunc(func(_ context.Context, got Exchange) (Message, error) {
				if got.Message.CommandType() != CommandTGSAlarm {
					t.Fatalf("consumer command = %s", got.Message.CommandType())
				}
				return &NotificationResponse{CmdType: CommandTGSAlarm, SN: 21, Result: ResultOK}, nil
			}),
		}
		response, err := adapter.Handle(context.Background(), Version2011, exchange, body)
		if err != nil {
			t.Fatal(err)
		}
		if response == nil || response.CommandType() != CommandTGSAlarm {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("wrong response SN", func(t *testing.T) {
		adapter := Adapter{
			Authorizer: authorizerFunc(func(context.Context, Exchange) error { return nil }),
			Consumer: consumerFunc(func(context.Context, Exchange) (Message, error) {
				return &NotificationResponse{CmdType: CommandTGSAlarm, SN: 22, Result: ResultOK}, nil
			}),
		}
		if _, err := adapter.Handle(context.Background(), Version2011, exchange, body); err == nil {
			t.Fatal("Handle() accepted a response with the wrong SN")
		}
	})
}

func TestAdapterPrepareConsumeContract(t *testing.T) {
	body := []byte(`<Notify><CmdType>TGSAlarm</CmdType><SN>21</SN><AlarmContent><AlarmTime>2026-08-27T10:20:30</AlarmTime><TollgateID>gate</TollgateID><CarPlate>plate</CarPlate><PlateType>02</PlateType><DefenceType>wanted</DefenceType></AlarmContent></Notify>`)
	exchange := testExchange(RoleTollgateSystem, RoleManagementPlatform)
	authorized := 0
	consumed := 0
	adapter := Adapter{
		Authorizer: authorizerFunc(func(context.Context, Exchange) error {
			authorized++
			return nil
		}),
		Consumer: consumerFunc(func(context.Context, Exchange) (Message, error) {
			consumed++
			return &NotificationResponse{CmdType: CommandTGSAlarm, SN: 21, Result: ResultOK}, nil
		}),
	}

	prepared, err := adapter.Prepare(context.Background(), Version2011, exchange, body)
	if err != nil {
		t.Fatal(err)
	}
	if authorized != 1 || consumed != 0 {
		t.Fatalf("Prepare calls = authorized:%d consumed:%d, want 1,0", authorized, consumed)
	}

	response, err := adapter.Consume(context.Background(), Version2011, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || authorized != 2 || consumed != 1 {
		t.Fatalf("Consume result = %#v, calls = authorized:%d consumed:%d, want response,2,1", response, authorized, consumed)
	}

	authorized = 0
	consumed = 0
	response, err = adapter.Handle(context.Background(), Version2011, exchange, body)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || authorized != 1 || consumed != 1 {
		t.Fatalf("Handle result = %#v, calls = authorized:%d consumed:%d, want response,1,1", response, authorized, consumed)
	}
}

func TestErrorResponse(t *testing.T) {
	requests := []Message{
		&TGSAlarmNotify{CmdType: CommandTGSAlarm, SN: 41, AlarmContent: testTGSRecord()},
		&AlarmRecordQuery{CmdType: CommandMPAlarmRecordList, SN: 42},
	}
	for _, request := range requests {
		response := ErrorResponse(request)
		if response == nil {
			t.Fatalf("ErrorResponse(%T) = nil", request)
		}
		if response.RootName() != "Response" || response.CommandType() != request.CommandType() || messageSN(response) != messageSN(request) {
			t.Fatalf("ErrorResponse(%T) = %#v", request, response)
		}
		if _, err := Encode(Version2011, response); err != nil {
			t.Fatalf("Encode(ErrorResponse(%T)) error = %v", request, err)
		}
	}
	if response := ErrorResponse(&NotificationResponse{CmdType: CommandMPAlarm, SN: 43, Result: ResultOK}); response != nil {
		t.Fatalf("ErrorResponse(Response) = %#v, want nil", response)
	}
}

func TestExchangeDirectionMatrix(t *testing.T) {
	tests := []struct {
		name        string
		root        string
		command     Command
		source      SystemRole
		destination SystemRole
	}{
		{"MP notify to ECS", "Notify", CommandMPAlarm, RoleManagementPlatform, RoleEmergencyCommandSystem},
		{"MP notify to city", "Notify", CommandMPAlarm, RoleManagementPlatform, RoleCityInformationSystem},
		{"ECS notify", "Notify", CommandECSAlarm, RoleEmergencyCommandSystem, RoleManagementPlatform},
		{"TGS notify", "Notify", CommandTGSAlarm, RoleTollgateSystem, RoleManagementPlatform},
		{"defence notify", "Notify", CommandConfigDefence, RoleManagementPlatform, RoleTollgateSystem},
		{"MP query from ECS", "Query", CommandMPAlarmRecordList, RoleEmergencyCommandSystem, RoleManagementPlatform},
		{"MP query from city", "Query", CommandMPAlarmRecordList, RoleCityInformationSystem, RoleManagementPlatform},
		{"ECS query", "Query", CommandECSAlarmRecordList, RoleManagementPlatform, RoleEmergencyCommandSystem},
		{"TGS query", "Query", CommandTGSAlarmRecordList, RoleManagementPlatform, RoleTollgateSystem},
		{"MP notification response", "Response", CommandMPAlarm, RoleEmergencyCommandSystem, RoleManagementPlatform},
		{"ECS notification response", "Response", CommandECSAlarm, RoleManagementPlatform, RoleEmergencyCommandSystem},
		{"TGS notification response", "Response", CommandTGSAlarm, RoleManagementPlatform, RoleTollgateSystem},
		{"defence response", "Response", CommandConfigDefence, RoleTollgateSystem, RoleManagementPlatform},
		{"MP query response", "Response", CommandMPAlarmRecordList, RoleManagementPlatform, RoleEmergencyCommandSystem},
		{"ECS query response", "Response", CommandECSAlarmRecordList, RoleEmergencyCommandSystem, RoleManagementPlatform},
		{"TGS query response", "Response", CommandTGSAlarmRecordList, RoleTollgateSystem, RoleManagementPlatform},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exchange := testExchange(test.source, test.destination)
			exchange.Message = directionMessage(test.root, test.command)
			if err := exchange.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			exchange.SourceRole, exchange.DestinationRole = exchange.DestinationRole, exchange.SourceRole
			if err := exchange.Validate(); err == nil {
				t.Fatal("Validate() accepted the reversed direction")
			}
		})
	}
}

func directionMessage(root string, command Command) Message {
	switch root {
	case "Notify":
		switch command {
		case CommandMPAlarm:
			return &MPAlarmNotify{CmdType: command}
		case CommandECSAlarm:
			return &ECSAlarmNotify{CmdType: command}
		case CommandTGSAlarm:
			return &TGSAlarmNotify{CmdType: command}
		case CommandConfigDefence:
			return &ConfigDefenceNotify{CmdType: command}
		}
	case "Query":
		return &AlarmRecordQuery{CmdType: command}
	case "Response":
		switch command {
		case CommandMPAlarm, CommandECSAlarm, CommandTGSAlarm, CommandConfigDefence:
			return &NotificationResponse{CmdType: command}
		case CommandMPAlarmRecordList:
			return &MPAlarmRecordListResponse{CmdType: command}
		case CommandECSAlarmRecordList:
			return &ECSAlarmRecordListResponse{CmdType: command}
		case CommandTGSAlarmRecordList:
			return &TGSAlarmRecordListResponse{CmdType: command}
		}
	}
	return nil
}

type mpAlarmStoreStub struct {
	saved   int
	queried int
}

func (store *mpAlarmStoreStub) SaveMPAlarmRecord(context.Context, MPAlarmRecord) error {
	store.saved++
	return nil
}

func (store *mpAlarmStoreStub) QueryMPAlarmRecords(_ context.Context, query AlarmRecordQuery) (MPAlarmRecordListResponse, error) {
	store.queried++
	return MPAlarmRecordListResponse{
		CmdType: CommandMPAlarmRecordList, SN: query.SN, Result: ResultOK, RealRecordNum: 1, SendRecordNum: 1,
		RecordList: MPAlarmRecordList{AlarmRecords: []MPAlarmRecord{testMPRecord()}},
	}, nil
}

type defenceStoreStub struct {
	applied int
}

func (store *defenceStoreStub) ApplyConfigDefence(_ context.Context, notify ConfigDefenceNotify) (NotificationResponse, error) {
	store.applied++
	return NotificationResponse{CmdType: CommandConfigDefence, SN: notify.SN, Result: ResultOK}, nil
}

func TestDomainConsumerDispatch(t *testing.T) {
	mpStore := &mpAlarmStoreStub{}
	defenceStore := &defenceStoreStub{}
	consumer := DomainConsumer{MPAlarmSink: mpStore, MPAlarmQuerier: mpStore, Defence: defenceStore}

	t.Run("MP notification", func(t *testing.T) {
		notify := &MPAlarmNotify{CmdType: CommandMPAlarm, SN: 31, AlarmContent: testMPRecord()}
		response, err := consumer.ConsumeAnnexG(context.Background(), Exchange{Message: notify})
		if err != nil {
			t.Fatal(err)
		}
		ack, ok := response.(*NotificationResponse)
		if !ok || ack.SN != 31 || ack.CmdType != CommandMPAlarm || ack.Result != ResultOK || mpStore.saved != 1 {
			t.Fatalf("response = %#v, saved = %d", response, mpStore.saved)
		}
	})

	t.Run("MP query", func(t *testing.T) {
		query := &AlarmRecordQuery{CmdType: CommandMPAlarmRecordList, SN: 32}
		response, err := consumer.ConsumeAnnexG(context.Background(), Exchange{Message: query})
		if err != nil {
			t.Fatal(err)
		}
		result, ok := response.(*MPAlarmRecordListResponse)
		if !ok || result.SN != 32 || mpStore.queried != 1 {
			t.Fatalf("response = %#v, queried = %d", response, mpStore.queried)
		}
	})

	t.Run("defence", func(t *testing.T) {
		notify := &ConfigDefenceNotify{CmdType: CommandConfigDefence, SN: 33}
		response, err := consumer.ConsumeAnnexG(context.Background(), Exchange{Message: notify})
		if err != nil {
			t.Fatal(err)
		}
		ack, ok := response.(*NotificationResponse)
		if !ok || ack.SN != 33 || defenceStore.applied != 1 {
			t.Fatalf("response = %#v, applied = %d", response, defenceStore.applied)
		}
	})

	t.Run("response no-op", func(t *testing.T) {
		response, err := consumer.ConsumeAnnexG(context.Background(), Exchange{Message: &NotificationResponse{CmdType: CommandMPAlarm}})
		if err != nil || response != nil {
			t.Fatalf("ConsumeAnnexG() = %#v, %v; want nil, nil", response, err)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		_, err := consumer.ConsumeAnnexG(context.Background(), Exchange{Message: &ECSAlarmNotify{CmdType: CommandECSAlarm}})
		if !errors.Is(err, ErrConsumerUnavailable) {
			t.Fatalf("error = %v, want ErrConsumerUnavailable", err)
		}
	})
}
