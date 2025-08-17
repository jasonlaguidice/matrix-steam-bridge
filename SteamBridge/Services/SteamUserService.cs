using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

public class SteamUserService : Proto.SteamUserService.SteamUserServiceBase
{
    private readonly ILogger<SteamUserService> _logger;
    private readonly SteamUserInformationService _userInfoService;

    public SteamUserService(
        ILogger<SteamUserService> logger,
        SteamUserInformationService userInfoService)
    {
        _logger = logger;
        _userInfoService = userInfoService;
    }

    public override async Task<UserInfoResponse> GetUserInfo(
        UserInfoRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received user info request for SteamID: {SteamID}", 
            request.SteamId == 0 ? "current user" : request.SteamId.ToString());

        try
        {
            var steamId = request.SteamId == 0 ? null : (ulong?)request.SteamId;
            var userInfo = await _userInfoService.GetUserInfoAsync(steamId);

            var response = new UserInfoResponse();
            
            if (userInfo != null)
            {
                response.UserInfo = MapToProtoUserInfo(userInfo);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing user info request");
            throw new RpcException(new Status(StatusCode.Internal, $"Internal error: {ex.Message}"));
        }
    }

    public override async Task<FriendsListResponse> GetFriendsList(
        FriendsListRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received friends list request");

        try
        {
            var friends = await _userInfoService.GetFriendsListAsync();
            var response = new FriendsListResponse();
            
            foreach (var friend in friends)
            {
                response.Friends.Add(MapToProtoFriend(friend));
            }

            _logger.LogInformation("Returning {FriendCount} friends", response.Friends.Count);
            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing friends list request");
            throw new RpcException(new Status(StatusCode.Internal, $"Internal error: {ex.Message}"));
        }
    }

    public override async Task<UserStatusResponse> GetUserStatus(
        UserStatusRequest request, 
        ServerCallContext context)
    {
        _logger.LogDebug("Received user status request for SteamID: {SteamID}", request.SteamId);

        try
        {
            var status = await _userInfoService.GetUserStatusAsync(request.SteamId);
            
            return new UserStatusResponse
            {
                Status = MapToProtoPersonaState(status),
                LastOnline = DateTimeOffset.UtcNow.ToUnixTimeSeconds() // Simplified - would need actual last online time
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing user status request");
            throw new RpcException(new Status(StatusCode.Internal, $"Internal error: {ex.Message}"));
        }
    }

    public override async Task<ResolveVanityURLResponse> ResolveVanityURL(
        ResolveVanityURLRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received vanity URL resolution request for: {VanityUrl}", request.VanityUrl);

        try
        {
            if (string.IsNullOrWhiteSpace(request.VanityUrl))
            {
                _logger.LogWarning("Empty vanity URL provided");
                return new ResolveVanityURLResponse
                {
                    Success = false,
                    ErrorMessage = "Vanity URL cannot be empty"
                };
            }

            var (success, steamId, errorMessage) = await _userInfoService.ResolveVanityUrlAsync(request.VanityUrl);
            
            var response = new ResolveVanityURLResponse
            {
                Success = success,
                ErrorMessage = errorMessage ?? string.Empty
            };

            if (success && !string.IsNullOrEmpty(steamId))
            {
                response.SteamId = steamId;
                _logger.LogInformation("Successfully resolved vanity URL '{VanityUrl}' to SteamID: {SteamId}", 
                    request.VanityUrl, steamId);
            }
            else
            {
                _logger.LogWarning("Failed to resolve vanity URL '{VanityUrl}': {Error}", 
                    request.VanityUrl, errorMessage);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing vanity URL resolution request");
            throw new RpcException(new Status(StatusCode.Internal, $"Internal error: {ex.Message}"));
        }
    }

    private static Proto.UserInfo MapToProtoUserInfo(Services.UserInfo userInfo)
    {
        return new Proto.UserInfo
        {
            SteamId = userInfo.SteamId,
            AccountName = userInfo.AccountName,
            PersonaName = userInfo.PersonaName,
            ProfileUrl = userInfo.ProfileUrl,
            AvatarUrl = userInfo.AvatarUrl,
            AvatarHash = ExtractAvatarHash(userInfo.AvatarUrl),
            Status = MapToProtoPersonaState(userInfo.Status),
            CurrentGame = userInfo.CurrentGame
        };
    }

    private static Proto.Friend MapToProtoFriend(Services.Friend friend)
    {
        return new Proto.Friend
        {
            SteamId = friend.SteamId,
            PersonaName = friend.PersonaName,
            AvatarUrl = friend.AvatarUrl,
            AvatarHash = ExtractAvatarHash(friend.AvatarUrl),
            Status = MapToProtoPersonaState(friend.Status),
            CurrentGame = friend.CurrentGame,
            Relationship = MapToProtoFriendRelationship(friend.Relationship)
        };
    }

    private static string ExtractAvatarHash(string avatarUrl)
    {
        if (string.IsNullOrEmpty(avatarUrl))
            return string.Empty;

        try
        {
            // Steam avatar URLs look like: https://avatars.steamstatic.com/b5bd56c1aa4644a474a2e4972be27ef9e82e517e_full.jpg
            // Extract the hash part before the suffix
            var uri = new Uri(avatarUrl);
            var filename = Path.GetFileNameWithoutExtension(uri.AbsolutePath);
            
            // Remove the size suffix (_full, _medium, etc.) to get just the hash
            var underscoreIndex = filename.LastIndexOf('_');
            if (underscoreIndex > 0)
            {
                return filename.Substring(0, underscoreIndex);
            }
            
            return filename;
        }
        catch
        {
            // If URL parsing fails, return empty string
            return string.Empty;
        }
    }

    private static Proto.PersonaState MapToProtoPersonaState(Services.PersonaState state)
    {
        return state switch
        {
            Services.PersonaState.Offline => Proto.PersonaState.Offline,
            Services.PersonaState.Online => Proto.PersonaState.Online,
            Services.PersonaState.Busy => Proto.PersonaState.Busy,
            Services.PersonaState.Away => Proto.PersonaState.Away,
            Services.PersonaState.Snooze => Proto.PersonaState.Snooze,
            Services.PersonaState.LookingToTrade => Proto.PersonaState.LookingToTrade,
            Services.PersonaState.LookingToPlay => Proto.PersonaState.LookingToPlay,
            Services.PersonaState.Invisible => Proto.PersonaState.Invisible,
            _ => Proto.PersonaState.Offline
        };
    }

    private static Proto.FriendRelationship MapToProtoFriendRelationship(Services.FriendRelationship relationship)
    {
        return relationship switch
        {
            Services.FriendRelationship.None => Proto.FriendRelationship.None,
            Services.FriendRelationship.Blocked => Proto.FriendRelationship.Blocked,
            Services.FriendRelationship.RequestRecipient => Proto.FriendRelationship.RequestRecipient,
            Services.FriendRelationship.Friend => Proto.FriendRelationship.Friend,
            Services.FriendRelationship.RequestInitiator => Proto.FriendRelationship.RequestInitiator,
            Services.FriendRelationship.Ignored => Proto.FriendRelationship.Ignored,
            Services.FriendRelationship.IgnoredFriend => Proto.FriendRelationship.IgnoredFriend,
            _ => Proto.FriendRelationship.None
        };
    }
}