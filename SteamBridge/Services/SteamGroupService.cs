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

                foreach (var chatRoom in summary.chat_rooms)
                {
                    group.Channels.Add(new ChatChannel
                    {
                        ChatId = chatRoom.chat_id,
                        Name = chatRoom.chat_name ?? string.Empty,
                    });
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

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error getting chat room groups");
            return new GetGroupsResponse { Success = false, ErrorMessage = ex.Message };
        }
    }
}
