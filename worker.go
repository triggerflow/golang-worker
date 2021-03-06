package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"triggerflow/config"
	"triggerflow/eventsource"
	"triggerflow/tirggerstorage"
	"triggerflow/trigger"

	cloudevents "github.com/cloudevents/sdk-go"
	log "github.com/sirupsen/logrus"
)

type Workspace struct {
	WorkspaceName       string
	Triggers            trigger.Map
	TriggerEventMapping trigger.ActivationEventMap
	GlobalContext       map[string]map[string]interface{}
	TriggerStorage      tirggerstorage.Storage
	EventSources        map[string]eventsource.EventSource
	EventSink           chan *cloudevents.Event
	CheckpointChannel   chan *trigger.Trigger
}

func ProcessWorkspace(workspaceName string) {
	workspace := Workspace{
		WorkspaceName:       workspaceName,
		Triggers:            make(trigger.Map),
		TriggerEventMapping: make(trigger.ActivationEventMap),
		EventSources:        make(map[string]eventsource.EventSource),
		EventSink:           make(chan *cloudevents.Event, config.SinkMaxSize),
		GlobalContext:       make(map[string]map[string]interface{}),
		CheckpointChannel:   make(chan *trigger.Trigger),
	}

	workspace.startTriggerStorage()

	workspaces := workspace.TriggerStorage.Get("triggerflow", "workspaces")
	if _, ok := workspaces[workspaceName]; !ok {
		panic(errors.New(fmt.Sprintf("Workspace %s is not defined", workspaceName)))
	}

	globalContext := workspace.TriggerStorage.Get(workspaceName, "global_context")
	for key, value := range globalContext {
		parsedValue := make(map[string]interface{})
		err := json.Unmarshal([]byte(value), &parsedValue)
		if err != nil {
			panic(err)
		}
		workspace.GlobalContext[key] = parsedValue
	}

	workspace.startEventSources()
	workspace.updateTriggers()

	for _, trg := range workspace.Triggers {
		go workspace.processTrigger(trg)
	}

	for event := range workspace.EventSink {
		if matchingTriggers, ok := workspace.TriggerEventMapping[event.Subject()][event.Type()]; ok {
			for _, trg := range matchingTriggers {
				trg.EventChannel <- event
			}
		} else {
			log.Infof("Received event with subject <%s> and type <%s> not found in local trigger cache", event.Subject(), event.Type())
			workspace.updateTriggers()
			workspace.EventSink <- event
		}
	}
}

func (workspace *Workspace) processTrigger(trg *trigger.Trigger) {
	for event := range trg.EventChannel {
		log.Debugf("Processing trigger <%s>", trg.TriggerID)
		condition, err := trg.Condition(trg.Context, *event)

		if err != nil {
			log.Errorf("Error while processing <%s> condition: %s", trg.TriggerID, err)
			return
		}

		if condition {
			err = trg.Action(trg.Context, *event)
			if err != nil {
				log.Errorf("Error while processing <%s> action: %s", trg.TriggerID, err)
				return
			}

			log.Infof("Trigger %s action fired", trg.TriggerID)
			go workspace.checkpointTriggers()
		}
	}
}

func (workspace *Workspace) updateTriggers() {
	log.Infof("Updating trigger cache...")
	allTriggers := workspace.TriggerStorage.Get((*workspace).WorkspaceName, "triggers")

	for triggerID, triggerJSON := range allTriggers {
		if _, ok := workspace.Triggers[triggerID]; !ok {

			newTrigger, err := trigger.UnmarshalJSONTrigger([]byte(triggerJSON))
			if err != nil {
				log.Errorf("Encountered error during JSON Trigger unmarshal: %s", err)
				continue
			}

			workspace.contextualizeTrigger(newTrigger)
			workspace.Triggers[newTrigger.TriggerID] = newTrigger

			for _, actEvt := range newTrigger.ActivationEvents {
				if _, ok := workspace.TriggerEventMapping[actEvt.Subject()]; !ok {
					workspace.TriggerEventMapping[actEvt.Subject()] = make(map[string][]*trigger.Trigger)
				}

				if _, ok := workspace.TriggerEventMapping[actEvt.Subject()][actEvt.Type()]; !ok {
					workspace.TriggerEventMapping[actEvt.Subject()][actEvt.Type()] = make([]*trigger.Trigger, 0)
				}

				trgIDs := workspace.TriggerEventMapping[actEvt.Subject()][actEvt.Type()]
				workspace.TriggerEventMapping[actEvt.Subject()][actEvt.Type()] = append(trgIDs, newTrigger)
			}

			log.Debugf("Added new trigger to cache: <%s> <%s>", newTrigger.TriggerID, newTrigger.UUID)
		}
	}

	log.Infof("Triggers updated -- %d triggers in local cache", len(workspace.Triggers))
}

func (workspace *Workspace) contextualizeTrigger(trg *trigger.Trigger) {
	var err error

	(*trg).Context.EventSink = workspace.EventSink
	(*trg).Context.EventSources = workspace.EventSources
	(*trg).Context.Triggers = workspace.Triggers
	(*trg).Context.TriggerEventMapping = workspace.TriggerEventMapping
	(*trg).Context.GlobalContext = workspace.GlobalContext

	conditionParser := (*trg).ConditionFunctionData["name"]
	conditionFunctionParser := trigger.ContextParsers[conditionParser]
	if conditionFunctionParser != nil {
		(*trg).Context.ConditionParsedData, err = conditionFunctionParser((*trg).Context.RawData)

		if err != nil {
			panic(err)
		}
	}

	actionParser := (*trg).ActionFunctionData["name"]
	actionFunctionParser := trigger.ContextParsers[actionParser]
	if actionFunctionParser != nil {
		(*trg).Context.ActionParsedData, err = actionFunctionParser((*trg).Context.RawData)

		if err != nil {
			panic(err)
		}
	}
}

func (workspace *Workspace) startTriggerStorage() {
	TriggerStorage := tirggerstorage.BackendConstructors[config.Map.TriggerStorage.Backend]
	workspace.TriggerStorage = TriggerStorage(config.Map.TriggerStorage.Parameters)
}

func (workspace *Workspace) startEventSources() {
	eventSources := workspace.TriggerStorage.Get(workspace.WorkspaceName, "event_sources")

	for _, evtSourceJSON := range eventSources {
		eventSourceMeta := struct {
			Class      string
			Name       string
			Parameters json.RawMessage
		}{}

		if err := json.Unmarshal([]byte(evtSourceJSON), &eventSourceMeta); err != nil {
			panic(err)
		}

		// Instantiate EventSource and start consuming events
		EventSource := eventsource.Constructors[eventSourceMeta.Class]
		workspace.EventSources[eventSourceMeta.Name] = EventSource(workspace.WorkspaceName, workspace.EventSink, eventSourceMeta.Parameters)
		go workspace.EventSources[eventSourceMeta.Name].StartConsuming()
	}
}

func (workspace *Workspace) checkpointTriggers() {
	for _, eventSource := range workspace.EventSources {
		go eventSource.CommitEvents()
	}

	for trg := range workspace.CheckpointChannel {
		encodedTrigger, err := trigger.MarshalJSONTrigger(trg)
		if err != nil {
			log.Errorf("Could not checkpoint trigger %s", trg.TriggerID)
		} else {
			go workspace.TriggerStorage.Put(workspace.WorkspaceName, "triggers", trg.TriggerID, encodedTrigger)
		}
	}
}
