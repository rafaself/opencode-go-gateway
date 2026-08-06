# Reduce Codex JSONL to a safe event-shape record.  Never copy text, command
# arguments, tool inputs/outputs, IDs, paths, or usage values into the result.
def raw_event_type:
  if (.type? | type) == "string" then .type else "" end;

def raw_item_type:
  if ((.item? | type) == "object") and ((.item.type? | type) == "string") then
    .item.type
  else
    ""
  end;

def known_event_type($value):
  if $value == "thread.started" or
     $value == "turn.started" or
     $value == "turn.completed" or
     $value == "turn.failed" or
     $value == "turn.interrupted" or
     $value == "item.started" or
     $value == "item.updated" or
     $value == "item.completed" or
     $value == "item.delta" or
     $value == "error" then
    $value
  else
    "other"
  end;

def known_item_type($value):
  if $value == "agent_message" or
     $value == "message" or
     $value == "reasoning" or
     $value == "command_execution" or
     $value == "function_call" or
     $value == "custom_tool_call" or
     $value == "apply_patch" or
     $value == "file_change" then
    $value
  else
    null
  end;

def phase_for($value):
  if ($value | endswith(".started")) then
    "started"
  elif ($value | endswith(".updated")) then
    "updated"
  elif ($value | endswith(".completed")) then
    "completed"
  elif ($value | endswith(".delta")) then
    "delta"
  else
    null
  end;

def safe_status_value($value):
  if $value == "pending" or
     $value == "in_progress" or
     $value == "completed" or
     $value == "failed" or
     $value == "incomplete" or
     $value == "interrupted" or
     $value == "cancelled" then
    $value
  else
    null
  end;

def safe_status:
  if ((.item? | type) == "object") and ((.item.status? | type) == "string") then
    safe_status_value(.item.status)
  elif ((.status? | type) == "string") then
    safe_status_value(.status)
  else
    null
  end;

(. | raw_event_type) as $event |
(. | raw_item_type) as $item |
(known_item_type($item)) as $known_item |
{
  event_type: known_event_type($event),
  item_type: $known_item,
  phase: phase_for($event),
  status: safe_status,
  tool_event: ($known_item == "command_execution" or
    $known_item == "function_call" or
    $known_item == "custom_tool_call" or
    $known_item == "apply_patch" or
    $known_item == "file_change"),
  has_item: ((.item? | type) == "object"),
  has_delta: ((.delta? | type) == "string")
}
