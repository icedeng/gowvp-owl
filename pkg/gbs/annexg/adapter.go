package annexg

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrAdapterDisabled = errors.New("Annex G adapter is disabled")
var ErrConsumerUnavailable = errors.New("Annex G domain consumer is unavailable")

// SystemRole 标识附录 G 定义的端点角色。
type SystemRole string

const (
	RoleManagementPlatform     SystemRole = "management_platform"
	RoleEmergencyCommandSystem SystemRole = "emergency_command_system"
	RoleTollgateSystem         SystemRole = "tollgate_system"
	RoleCityInformationSystem  SystemRole = "city_information_system"
)

// Exchange 将已校验消息绑定到已认证的源系统和目标系统。
// 传输适配器必须从可信配置构造该对象，不能只依赖不可信 XML 字段。
type Exchange struct {
	SourceID        string
	SourceRole      SystemRole
	DestinationID   string
	DestinationRole SystemRole
	Message         Message
}

// Authorizer 在 XML 和方向校验后执行产品侧外部系统身份及权限策略。
type Authorizer interface {
	AuthorizeAnnexG(context.Context, Exchange) error
}

// Consumer 将纯协议模块桥接到产品领域拥有的报警存储、布控状态、查询执行和响应关联。
// Query 或 Notify 通常返回对应 Response；消费入向 Response 时通常返回 nil。
type Consumer interface {
	ConsumeAnnexG(context.Context, Exchange) (Message, error)
}

// DomainConsumer 按消息类型调用最小领域存储接口，不负责身份授权和传输。
// 只需配置当前系统角色实际拥有的存储；收到未配置能力时明确拒绝。
type DomainConsumer struct {
	MPAlarmSink     MPAlarmSink
	MPAlarmQuerier  MPAlarmQuerier
	ECSAlarmSink    ECSAlarmSink
	ECSAlarmQuerier ECSAlarmQuerier
	TGSAlarmSink    TGSAlarmSink
	TGSAlarmQuerier TGSAlarmQuerier
	Defence         DefenceStore
}

// ConsumeAnnexG 把已校验的附录 G 消息分派给对应领域存储。
func (consumer DomainConsumer) ConsumeAnnexG(ctx context.Context, exchange Exchange) (Message, error) {
	switch message := exchange.Message.(type) {
	case *MPAlarmNotify:
		if consumer.MPAlarmSink == nil {
			return nil, ErrConsumerUnavailable
		}
		if err := consumer.MPAlarmSink.SaveMPAlarmRecord(ctx, message.AlarmContent); err != nil {
			return nil, err
		}
		return &NotificationResponse{CmdType: CommandMPAlarm, SN: message.SN, Result: ResultOK}, nil
	case *ECSAlarmNotify:
		if consumer.ECSAlarmSink == nil {
			return nil, ErrConsumerUnavailable
		}
		if err := consumer.ECSAlarmSink.SaveECSAlarmRecord(ctx, message.AlarmContent); err != nil {
			return nil, err
		}
		return &NotificationResponse{CmdType: CommandECSAlarm, SN: message.SN, Result: ResultOK}, nil
	case *TGSAlarmNotify:
		if consumer.TGSAlarmSink == nil {
			return nil, ErrConsumerUnavailable
		}
		if err := consumer.TGSAlarmSink.SaveTGSAlarmRecord(ctx, message.AlarmContent); err != nil {
			return nil, err
		}
		return &NotificationResponse{CmdType: CommandTGSAlarm, SN: message.SN, Result: ResultOK}, nil
	case *ConfigDefenceNotify:
		if consumer.Defence == nil {
			return nil, ErrConsumerUnavailable
		}
		response, err := consumer.Defence.ApplyConfigDefence(ctx, *message)
		return &response, err
	case *AlarmRecordQuery:
		switch message.CmdType {
		case CommandMPAlarmRecordList:
			if consumer.MPAlarmQuerier == nil {
				return nil, ErrConsumerUnavailable
			}
			response, err := consumer.MPAlarmQuerier.QueryMPAlarmRecords(ctx, *message)
			return &response, err
		case CommandECSAlarmRecordList:
			if consumer.ECSAlarmQuerier == nil {
				return nil, ErrConsumerUnavailable
			}
			response, err := consumer.ECSAlarmQuerier.QueryECSAlarmRecords(ctx, *message)
			return &response, err
		case CommandTGSAlarmRecordList:
			if consumer.TGSAlarmQuerier == nil {
				return nil, ErrConsumerUnavailable
			}
			response, err := consumer.TGSAlarmQuerier.QueryTGSAlarmRecords(ctx, *message)
			return &response, err
		}
	case *NotificationResponse, *MPAlarmRecordListResponse, *ECSAlarmRecordListResponse, *TGSAlarmRecordListResponse:
		return nil, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrConsumerUnavailable, exchange.Message)
}

// Adapter 在把消息交给产品代码前校验 XML、端点方向和授权。零值 Adapter 默认禁用。
type Adapter struct {
	Authorizer Authorizer
	Consumer   Consumer
}

// Handle 处理一次已经外部认证的附录 G 交换，不发送 SIP 消息；事务和传输行为由调用方负责。
func (adapter Adapter) Handle(ctx context.Context, version Version, exchange Exchange, body []byte) (Message, error) {
	prepared, err := adapter.Prepare(ctx, version, exchange, body)
	if err != nil {
		return nil, err
	}
	return adapter.consumePrepared(ctx, version, prepared)
}

// Prepare 完成 XML、版本、方向和权限校验，但不调用领域消费方。
// 传输层可在成功后先发送 SIP 确认，再异步执行 Consume。
func (adapter Adapter) Prepare(ctx context.Context, version Version, exchange Exchange, body []byte) (Exchange, error) {
	if adapter.Authorizer == nil || adapter.Consumer == nil {
		return Exchange{}, ErrAdapterDisabled
	}
	if ctx == nil {
		return Exchange{}, errors.New("nil Annex G context")
	}
	message, err := Decode(version, body)
	if err != nil {
		return Exchange{}, err
	}
	exchange.Message = message
	if err := exchange.Validate(); err != nil {
		return Exchange{}, err
	}
	if err := adapter.Authorizer.AuthorizeAnnexG(ctx, exchange); err != nil {
		return Exchange{}, fmt.Errorf("authorize Annex G exchange: %w", err)
	}
	return exchange, nil
}

// Consume 调用领域消费方，并校验其业务响应与原消息关联一致。
func (adapter Adapter) Consume(ctx context.Context, version Version, exchange Exchange) (Message, error) {
	if adapter.Authorizer == nil || adapter.Consumer == nil {
		return nil, ErrAdapterDisabled
	}
	if ctx == nil {
		return nil, errors.New("nil Annex G context")
	}
	if exchange.Message == nil {
		return nil, errors.New("Annex G exchange has no prepared message")
	}
	if err := exchange.Message.Validate(version); err != nil {
		return nil, err
	}
	if err := exchange.Validate(); err != nil {
		return nil, err
	}
	if err := adapter.Authorizer.AuthorizeAnnexG(ctx, exchange); err != nil {
		return nil, fmt.Errorf("authorize Annex G exchange: %w", err)
	}
	return adapter.consumePrepared(ctx, version, exchange)
}

func (adapter Adapter) consumePrepared(ctx context.Context, version Version, exchange Exchange) (Message, error) {
	response, err := adapter.Consumer.ConsumeAnnexG(ctx, exchange)
	if err != nil {
		return nil, fmt.Errorf("consume Annex G exchange: %w", err)
	}
	if response == nil {
		return nil, nil
	}
	if err := response.Validate(version); err != nil {
		return nil, fmt.Errorf("consumer returned invalid Annex G response: %w", err)
	}
	if err := validateResponseCorrelation(exchange, response); err != nil {
		return nil, err
	}
	return response, nil
}

// Validate 校验附录 G 规定的消息方向。
func (exchange Exchange) Validate() error {
	if exchange.Message == nil {
		return errors.New("Annex G exchange has no message")
	}
	if exchange.SourceRole == "" || exchange.DestinationRole == "" {
		return errors.New("Annex G exchange requires source and destination roles")
	}
	if strings.TrimSpace(exchange.SourceID) == "" || strings.TrimSpace(exchange.DestinationID) == "" {
		return errors.New("Annex G exchange requires authenticated source and destination identities")
	}
	allowed := false
	switch exchange.Message.RootName() {
	case "Notify":
		allowed = validNotifyDirection(exchange.Message.CommandType(), exchange.SourceRole, exchange.DestinationRole)
	case "Query":
		allowed = validQueryDirection(exchange.Message.CommandType(), exchange.SourceRole, exchange.DestinationRole)
	case "Response":
		allowed = validResponseDirection(exchange.Message.CommandType(), exchange.SourceRole, exchange.DestinationRole)
	}
	if !allowed {
		return fmt.Errorf("invalid Annex G direction %s -> %s for %s/%s", exchange.SourceRole, exchange.DestinationRole, exchange.Message.RootName(), exchange.Message.CommandType())
	}
	return nil
}

func validNotifyDirection(command Command, source, destination SystemRole) bool {
	switch command {
	case CommandMPAlarm:
		return source == RoleManagementPlatform && (destination == RoleEmergencyCommandSystem || destination == RoleCityInformationSystem)
	case CommandECSAlarm:
		return source == RoleEmergencyCommandSystem && destination == RoleManagementPlatform
	case CommandTGSAlarm:
		return source == RoleTollgateSystem && destination == RoleManagementPlatform
	case CommandConfigDefence:
		return source == RoleManagementPlatform && destination == RoleTollgateSystem
	default:
		return false
	}
}

func validQueryDirection(command Command, source, destination SystemRole) bool {
	switch command {
	case CommandMPAlarmRecordList:
		return (source == RoleEmergencyCommandSystem || source == RoleCityInformationSystem) && destination == RoleManagementPlatform
	case CommandECSAlarmRecordList:
		return source == RoleManagementPlatform && destination == RoleEmergencyCommandSystem
	case CommandTGSAlarmRecordList:
		return source == RoleManagementPlatform && destination == RoleTollgateSystem
	default:
		return false
	}
}

func validResponseDirection(command Command, source, destination SystemRole) bool {
	switch command {
	case CommandMPAlarm:
		return (source == RoleEmergencyCommandSystem || source == RoleCityInformationSystem) && destination == RoleManagementPlatform
	case CommandECSAlarm:
		return source == RoleManagementPlatform && destination == RoleEmergencyCommandSystem
	case CommandTGSAlarm:
		return source == RoleManagementPlatform && destination == RoleTollgateSystem
	case CommandConfigDefence:
		return source == RoleTollgateSystem && destination == RoleManagementPlatform
	case CommandMPAlarmRecordList:
		return source == RoleManagementPlatform && (destination == RoleEmergencyCommandSystem || destination == RoleCityInformationSystem)
	case CommandECSAlarmRecordList:
		return source == RoleEmergencyCommandSystem && destination == RoleManagementPlatform
	case CommandTGSAlarmRecordList:
		return source == RoleTollgateSystem && destination == RoleManagementPlatform
	default:
		return false
	}
}

func validateResponseCorrelation(exchange Exchange, response Message) error {
	if exchange.Message.RootName() == "Response" {
		return errors.New("Annex G consumer must not return a response to an inbound Response")
	}
	if response.RootName() != "Response" || response.CommandType() != exchange.Message.CommandType() {
		return fmt.Errorf("Annex G response command %s/%s does not match request %s/%s", response.RootName(), response.CommandType(), exchange.Message.RootName(), exchange.Message.CommandType())
	}
	wantSN := messageSN(exchange.Message)
	if gotSN := messageSN(response); gotSN != wantSN {
		return fmt.Errorf("Annex G response SN %d does not match request SN %d", gotSN, wantSN)
	}
	responseExchange := Exchange{
		SourceID: exchange.DestinationID, SourceRole: exchange.DestinationRole,
		DestinationID: exchange.SourceID, DestinationRole: exchange.SourceRole,
		Message: response,
	}
	if err := responseExchange.Validate(); err != nil {
		return fmt.Errorf("invalid Annex G response exchange: %w", err)
	}
	return nil
}

func messageSN(message Message) int {
	switch value := message.(type) {
	case *MPAlarmNotify:
		return value.SN
	case *ECSAlarmNotify:
		return value.SN
	case *TGSAlarmNotify:
		return value.SN
	case *ConfigDefenceNotify:
		return value.SN
	case *AlarmRecordQuery:
		return value.SN
	case *NotificationResponse:
		return value.SN
	case *MPAlarmRecordListResponse:
		return value.SN
	case *ECSAlarmRecordListResponse:
		return value.SN
	case *TGSAlarmRecordListResponse:
		return value.SN
	default:
		return 0
	}
}

// ErrorResponse 为已校验的通知或查询生成可编码的业务失败应答。
// 入向 Response 不允许再产生 Response，因此返回 nil。
func ErrorResponse(request Message) Message {
	if request == nil || request.RootName() == "Response" {
		return nil
	}
	sn := messageSN(request)
	switch request.CommandType() {
	case CommandMPAlarm, CommandECSAlarm, CommandTGSAlarm, CommandConfigDefence:
		return &NotificationResponse{CmdType: request.CommandType(), SN: sn, Result: ResultError}
	case CommandMPAlarmRecordList:
		return &MPAlarmRecordListResponse{CmdType: request.CommandType(), SN: sn, Result: ResultError}
	case CommandECSAlarmRecordList:
		return &ECSAlarmRecordListResponse{CmdType: request.CommandType(), SN: sn, Result: ResultError}
	case CommandTGSAlarmRecordList:
		return &TGSAlarmRecordListResponse{CmdType: request.CommandType(), SN: sn, Result: ResultError}
	default:
		return nil
	}
}
