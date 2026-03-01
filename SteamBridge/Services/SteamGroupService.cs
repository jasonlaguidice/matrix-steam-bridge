using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;
using SteamKit2.Internal;

namespace SteamBridge.Services;

public class SteamGroupService : Proto.SteamGroupService.SteamGroupServiceBase
{
    private readonly ILogger<SteamGroupService> _logger;
    private readonly SteamClientManager _steamClientManager;

    public SteamGroupService(ILogger<SteamGroupService> logger, SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
    }

    public override async Task<GetGroupsResponse> GetMyChatRoomGroups(
        GetGroupsRequest request,
        ServerCallContext context)
    {
        _logger.LogInformation("Getting user's chat room groups");

        if (!_steamClientManager.IsLoggedOn)
        {
            return new GetGroupsResponse { Success = false, ErrorMessage = "Not logged on" };
        }

        try
        {
            var chatRoomService = _steamClientManager.ChatRoomService;
            var job = chatRoomService.GetMyChatRoomGroups(new CChatRoom_GetMyChatRoomGroups_Request());
            var result = await job.ToTask();

            if (result == null || result.Result != SteamKit2.EResult.OK)
            {
                return new GetGroupsResponse
                {
                    Success = false,
                    ErrorMessage = $"Steam API error: {result?.Result}"
                };
            }

            var response = new GetGroupsResponse { Success = true };

            foreach (var summaryPair in result.Body.chat_room_groups)
            {
                var summary = summaryPair.group_summary;
                if (summary == null) continue;

                _logger.LogInformation(
                    "Group '{GroupName}' (id={GroupId}): default_chat_id={DefaultChatId}, chat_rooms count={ChannelCount}",
                    summary.chat_group_name,
                    summary.chat_group_id,
                    summary.default_chat_id,
                    summary.chat_rooms.Count);

                var group = new ChatGroup
                {
                    ChatGroupId = summary.chat_group_id,
                    Name = summary.chat_group_name ?? string.Empty,
                    Tagline = summary.chat_group_tagline ?? string.Empty,
                    Clanid = summary.clanid,
                    DirectMessagesAllowed = summaryPair.user_chat_group_state?.direct_messages_allowed ?? false,
                    DefaultChatId = summary.default_chat_id,
                };

                if (summary.chat_group_avatar_sha != null && summary.chat_group_avatar_sha.Length > 0)
                {
                    group.AvatarSha = Google.Protobuf.ByteString.CopyFrom(summary.chat_group_avatar_sha);
                }

                group.AvatarUrl = summary.avatar_ugc_url ?? string.Empty;

                foreach (var chatRoom in summary.chat_rooms)
                {
                    group.Channels.Add(new ChatChannel
                    {
                        ChatId = chatRoom.chat_id,
                        Name = chatRoom.chat_name ?? string.Empty,
                    });
                }

                // The default/Home channel (default_chat_id) is identified separately in the Steam API
                // and is often absent from the chat_rooms list. Ensure it is included so the Go bridge
                // can create a Matrix room for it.
                if (summary.default_chat_id != 0)
                {
                    bool defaultChannelPresent = group.Channels.Any(c => c.ChatId == summary.default_chat_id);
                    if (!defaultChannelPresent)
                    {
                        _logger.LogInformation(
                            "Group '{GroupName}' (id={GroupId}): default_chat_id={DefaultChatId} not in chat_rooms list; adding as 'General'",
                            summary.chat_group_name,
                            summary.chat_group_id,
                            summary.default_chat_id);

                        group.Channels.Add(new ChatChannel
                        {
                            ChatId = summary.default_chat_id,
                            Name = "General",
                        });
                    }
                }

                // Steam returns an empty chat_name for the default/home channel even when it is present
                // in the chat_rooms list. Ensure these channels are named "Home" instead of being
                // left empty (which causes the Go bridge to fall back to "channel-{id}").
                foreach (var channel in group.Channels)
                {
                    if (channel.ChatId == summary.default_chat_id && string.IsNullOrWhiteSpace(channel.Name))
                    {
                        _logger.LogInformation(
                            "Group '{GroupName}' (id={GroupId}): default channel (chat_id={ChatId}) has empty name; setting to 'Home'",
                            summary.chat_group_name,
                            summary.chat_group_id,
                            channel.ChatId);

                        channel.Name = "Home";
                    }
                }

                response.Groups.Add(group);
            }

            _logger.LogInformation("Returning {Count} chat groups", response.Groups.Count);

            // Register for group message notifications
            var groupIds = response.Groups.Select(g => g.ChatGroupId).ToList();
            var activateRequest = new CChatRoom_SetSessionActiveChatRoomGroups_Request();
            activateRequest.chat_group_ids.AddRange(groupIds);
            var activateJob = _steamClientManager.ChatRoomService.SetSessionActiveChatRoomGroups(activateRequest);
            await activateJob.ToTask();

            // Join each group to receive real-time CChatRoom_IncomingChatMessage_Notification events
            foreach (var group in response.Groups)
            {
                try
                {
                    var joinRequest = new CChatRoom_JoinChatRoomGroup_Request
                    {
                        chat_group_id = group.ChatGroupId
                    };
                    var joinJob = _steamClientManager.ChatRoomService.JoinChatRoomGroup(joinRequest);
                    await joinJob.ToTask();
                    _logger.LogDebug("Joined chat room group {GroupId} for real-time notifications", group.ChatGroupId);
                }
                catch (Exception ex)
                {
                    _logger.LogWarning(ex, "Failed to join chat room group {GroupId}, real-time messages may not arrive", group.ChatGroupId);
                }
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error getting chat room groups");
            return new GetGroupsResponse { Success = false, ErrorMessage = ex.Message };
        }
    }
}
